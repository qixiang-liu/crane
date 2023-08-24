package ehpa

import (
	"context"
	"fmt"

	autoscalingv2 "k8s.io/api/autoscaling/v2beta2"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/version"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/scale"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/yaml"

	autoscalingapi "github.com/gocrane/api/autoscaling/v1alpha1"
	predictionapi "github.com/gocrane/api/prediction/v1alpha1"

	"github.com/gocrane/crane/pkg/known"
	"github.com/gocrane/crane/pkg/metrics"
	"github.com/gocrane/crane/pkg/utils"
)

// EffectiveHPAController is responsible for scaling workload's replica based on EffectiveHorizontalPodAutoscaler spec
type EffectiveHPAController struct {
	client.Client
	Scheme      *runtime.Scheme
	RestMapper  meta.RESTMapper
	Recorder    record.EventRecorder
	ScaleClient scale.ScalesGetter
	K8SVersion  *version.Version
	Config      EhpaControllerConfig
}

func (c *EffectiveHPAController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	klog.V(4).Infof("Got ehpa %s", req.NamespacedName)

	// 更加参数 ctrl.Request 获取 EHPA 配置数据，如果已经删除直接退出本次后续逻辑
	ehpa := &autoscalingapi.EffectiveHorizontalPodAutoscaler{}
	err := c.Client.Get(ctx, req.NamespacedName, ehpa)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// 记录metrics数据，主要记录副本数统计数据
	RecordMetrics(ehpa)

	newStatus := ehpa.Status.DeepCopy()
	// 判断ehpa目标资源是否支持扩缩容，如果不支持，记录状态数据后，退出后续逻辑
	scale, mapping, err := utils.GetScale(ctx, c.RestMapper, c.ScaleClient, ehpa.Namespace, ehpa.Spec.ScaleTargetRef)
	if err != nil {
		c.Recorder.Event(ehpa, v1.EventTypeWarning, "FailedGetScale", err.Error())
		klog.Errorf("Failed to get scale, ehpa %s", klog.KObj(ehpa))
		setCondition(newStatus, autoscalingapi.Ready, metav1.ConditionFalse, "FailedGetScale", "Failed to get scale")
		c.UpdateStatus(ctx, ehpa, newStatus)
		return ctrl.Result{}, err
	}
	// 判断副本数配置，如果配置异常，记录状态数据后，退出后续逻辑
	if scale.Spec.Replicas == 0 && *ehpa.Spec.MinReplicas != 0 {
		newStatus.CurrentReplicas = &scale.Spec.Replicas
		setCondition(newStatus, autoscalingapi.Ready, metav1.ConditionFalse, "ScalingDisabled", "scaling is disabled since the replica count of the target is zero")
		c.UpdateStatus(ctx, ehpa, newStatus)
		return ctrl.Result{}, err
	}
	// 判断EHPA是不是预览(Preview)模式，如果是预览模式的话，控制器会创建或者更新Substitute资源对象
	var substitute *autoscalingapi.Substitute
	if ehpa.Spec.ScaleStrategy == autoscalingapi.ScaleStrategyPreview {
		substitute, err = c.ReconcileSubstitute(ctx, ehpa, scale)
		if err != nil {
			setCondition(newStatus, autoscalingapi.Ready, metav1.ConditionFalse, "FailedReconcileSubstitute", "Failed to reconcile substitute")
			c.UpdateStatus(ctx, ehpa, newStatus)
			return ctrl.Result{}, err
		}
	}
	// 判断EHPA是否开启，如果开启则创建或者更新对应的TimeSeriesPrediction资源对象
	// reconcile prediction if enabled
	var tsp *predictionapi.TimeSeriesPrediction
	if utils.IsEHPAPredictionEnabled(ehpa) && utils.IsEHPAHasPredictionMetric(ehpa) {
		tsp, err = c.ReconcilePredication(ctx, ehpa)
		if err != nil {
			setCondition(newStatus, autoscalingapi.Ready, metav1.ConditionFalse, "FailedReconcilePrediction", err.Error())
			c.UpdateStatus(ctx, ehpa, newStatus)
			return ctrl.Result{}, err
		}
		setPredictionCondition(newStatus, tsp.Status.Conditions)
	}
	// 创建对应的HPA资源对象
	hpa, err := c.ReconcileHPA(ctx, ehpa, substitute, tsp)
	if err != nil {
		setCondition(newStatus, autoscalingapi.Ready, metav1.ConditionFalse, "FailedReconcileHPA", err.Error())
		c.UpdateStatus(ctx, ehpa, newStatus)
		return ctrl.Result{}, err
	}
	// 更新当前EHPA状态数据
	newStatus.ExpectReplicas = &hpa.Status.DesiredReplicas
	newStatus.CurrentReplicas = &hpa.Status.CurrentReplicas

	if hpa.Status.LastScaleTime != nil && newStatus.LastScaleTime != nil && hpa.Status.LastScaleTime.After(newStatus.LastScaleTime.Time) {
		newStatus.LastScaleTime = hpa.Status.LastScaleTime
	}

	setHPACondition(newStatus, hpa.Status.Conditions)

	// 根据HPA的状态数据给当前EHPA增加注解数据
	// sync custom metric to annotations
	if hpa.Status.CurrentMetrics != nil {
		var currentMetrics string
		if ehpa.Annotations == nil {
			ehpa.Annotations = map[string]string{}
		}
		currentMetrics = ehpa.Annotations[known.EffectiveHorizontalPodAutoscalerCurrentMetricsAnnotation]

		valueBytes, err := yaml.Marshal(hpa.Status.CurrentMetrics)
		if err == nil && currentMetrics != string(valueBytes) {
			ehpa.Annotations[known.EffectiveHorizontalPodAutoscalerCurrentMetricsAnnotation] = string(valueBytes)
			klog.V(4).Infof("Updating ehpa %s current metrics: %s.", klog.KObj(ehpa), string(valueBytes))
			err := c.Client.Update(ctx, ehpa)
			if err != nil {
				klog.Errorf("Failed to update current metrics for ehpa %s: %v", klog.KObj(ehpa), err)
			}
			klog.Infof("Updated ehpa %s current metrics: %s.", klog.KObj(ehpa), string(valueBytes))
		}
	}

	// 更新状态数据和scale副本数配置
	// scale target to its specific replicas for Preview strategy
	if ehpa.Spec.ScaleStrategy == autoscalingapi.ScaleStrategyPreview && ehpa.Spec.SpecificReplicas != nil && *ehpa.Spec.SpecificReplicas != scale.Status.Replicas {
		scale.Spec.Replicas = *ehpa.Spec.SpecificReplicas
		updatedScale, err := c.ScaleClient.Scales(scale.Namespace).Update(ctx, mapping.Resource.GroupResource(), scale, metav1.UpdateOptions{})
		if err != nil {
			c.Recorder.Event(ehpa, v1.EventTypeWarning, "FailedManualScale", err.Error())
			msg := fmt.Sprintf("Failed to manual scale target to specific replicas, ehpa %s replicas %d", klog.KObj(ehpa), *ehpa.Spec.SpecificReplicas)
			klog.Error(err, msg)
			setCondition(newStatus, autoscalingapi.Ready, metav1.ConditionFalse, "FailedScale", msg)
			c.UpdateStatus(ctx, ehpa, newStatus)
			return ctrl.Result{}, err
		}

		klog.Infof("Manual scale target to specific replicas, ehpa %s replicas %d", klog.KObj(ehpa), *ehpa.Spec.SpecificReplicas)
		now := metav1.Now()
		newStatus.LastScaleTime = &now
		newStatus.CurrentReplicas = &updatedScale.Status.Replicas
	}

	setCondition(newStatus, autoscalingapi.Ready, metav1.ConditionTrue, "EffectiveHorizontalPodAutoscalerReady", "Effective HPA is ready")
	c.UpdateStatus(ctx, ehpa, newStatus)

	// 从上面的分析我们可以看到EHPA控制器会创建TimeSeriesPrediction资源，创建TimeSeriesPrediction资源后，TimeSeriesPrediction控制器就开始工作，会驱动对应的预测算法获取历史监控数据，进行副本数预测，病生成预测数据，metric-adapter会读取最终的预测数据，给HPA提供指标数据，从而干预影响workload的的扩缩容。
	//
	//
	//————————————————
	//版权声明：本文为CSDN博主「程序员大兵(lbbniu)」的原创文章，遵循CC 4.0 BY-SA版权协议，转载请附上原文出处链接及本声明。
	//原文链接：https://blog.csdn.net/lbbniu/article/details/130809473
	return ctrl.Result{}, nil
}

func (c *EffectiveHPAController) UpdateStatus(ctx context.Context, ehpa *autoscalingapi.EffectiveHorizontalPodAutoscaler, newStatus *autoscalingapi.EffectiveHorizontalPodAutoscalerStatus) {
	if !equality.Semantic.DeepEqual(&ehpa.Status, newStatus) {
		ehpaCopy := ehpa.DeepCopy()
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			ehpaCopy.Status = *newStatus
			err := c.Status().Update(ctx, ehpaCopy)
			if err == nil {
				return nil
			}

			updated := &autoscalingapi.EffectiveHorizontalPodAutoscaler{}
			errGet := c.Get(context.TODO(), types.NamespacedName{Namespace: ehpaCopy.Namespace, Name: ehpaCopy.Name}, updated)
			if errGet == nil {
				ehpaCopy = updated
			}

			return err

		})

		if err != nil {
			c.Recorder.Event(ehpa, v1.EventTypeWarning, "FailedUpdateStatus", err.Error())
			klog.Errorf("Failed to update status, EffectiveHorizontalPodAutoscaler %s error %v", klog.KObj(ehpa), err)
			return
		}

		klog.V(2).Infof("Update EffectiveHorizontalPodAutoscaler %s status successful ", klog.KObj(ehpa))
	}
}

func (c *EffectiveHPAController) SetupWithManager(mgr ctrl.Manager) error {
	discoveryClientSet, err := discovery.NewDiscoveryClientForConfig(mgr.GetConfig())
	if err != nil {
		return err
	}
	serverVersion, err := discoveryClientSet.ServerVersion()
	if err != nil {
		return err
	}
	K8SVersion, err := version.ParseGeneric(serverVersion.GitVersion)
	if err != nil {
		return err
	}
	c.K8SVersion = K8SVersion
	// 最核心的一行代码如下。
	return ctrl.NewControllerManagedBy(mgr).
		For(&autoscalingapi.EffectiveHorizontalPodAutoscaler{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&autoscalingv2.HorizontalPodAutoscaler{}).
		Owns(&predictionapi.TimeSeriesPrediction{}).
		Complete(c)
}

func setCondition(status *autoscalingapi.EffectiveHorizontalPodAutoscalerStatus, conditionType autoscalingapi.ConditionType, conditionStatus metav1.ConditionStatus, reason string, message string) {
	for i := range status.Conditions {
		if status.Conditions[i].Type == string(conditionType) {
			status.Conditions[i].Status = conditionStatus
			status.Conditions[i].Reason = reason
			status.Conditions[i].Message = message
			status.Conditions[i].LastTransitionTime = metav1.Now()
			return
		}
	}

	status.Conditions = append(status.Conditions, metav1.Condition{
		Type:               string(conditionType),
		Status:             conditionStatus,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.Now(),
	})
}

func RecordMetrics(ehpa *autoscalingapi.EffectiveHorizontalPodAutoscaler) {
	if ehpa.Status.ExpectReplicas != nil {
		labels := map[string]string{
			"namespace": ehpa.Namespace,
			"name":      ehpa.Name,
		}
		metrics.EHPAReplicas.With(labels).Set(float64(*ehpa.Status.ExpectReplicas))
	}
}
