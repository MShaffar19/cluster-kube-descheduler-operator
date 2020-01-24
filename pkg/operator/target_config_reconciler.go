package operator

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	deschedulerv1beta1 "github.com/openshift/cluster-kube-descheduler-operator/pkg/apis/descheduler/v1beta1"
	operatorconfigclientv1beta1 "github.com/openshift/cluster-kube-descheduler-operator/pkg/generated/clientset/versioned/typed/descheduler/v1beta1"
	operatorclientinformers "github.com/openshift/cluster-kube-descheduler-operator/pkg/generated/informers/externalversions/descheduler/v1beta1"
	"github.com/openshift/cluster-kube-descheduler-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-kube-descheduler-operator/pkg/operator/v410_00_assets"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/resource/resourceread"

	"gopkg.in/yaml.v2"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"
	deschedulerapi "sigs.k8s.io/descheduler/pkg/api/v1alpha1"
)

const DefaultImage = "quay.io/openshift/origin-descheduler:latest"

// array of valid strategies. TODO: Make this map(or set) once we have lot of strategies.
var validStrategies = sets.NewString("duplicates", "interpodantiaffinity", "lownodeutilization", "nodeaffinity")

// deschedulerCommand provides descheduler command with policyconfigfile mounted as volume and log-level for backwards
// compatibility with 3.11
var DeschedulerCommand = []string{"/bin/descheduler", "--policy-config-file", "/policy-dir/policy.yaml", "--v", "5"}

type TargetConfigReconciler struct {
	operatorClient operatorconfigclientv1beta1.KubedeschedulersV1beta1Interface
	kubeClient     kubernetes.Interface
	eventRecorder  events.Recorder
	queue          workqueue.RateLimitingInterface
}

func NewTargetConfigReconciler(
	operatorConfigClient operatorconfigclientv1beta1.KubedeschedulersV1beta1Interface,
	operatorClientInformer operatorclientinformers.KubeDeschedulerInformer,
	kubeClient kubernetes.Interface,
	eventRecorder events.Recorder,
) *TargetConfigReconciler {
	c := &TargetConfigReconciler{
		operatorClient: operatorConfigClient,
		kubeClient:     kubeClient,
		eventRecorder:  eventRecorder,
		queue:          workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "TargetConfigReconciler"),
	}

	operatorClientInformer.Informer().AddEventHandler(c.eventHandler())

	return c
}

func (c TargetConfigReconciler) sync() error {
	descheduler, err := c.operatorClient.KubeDeschedulers(operatorclient.OperatorNamespace).Get("cluster", metav1.GetOptions{})
	if err != nil {
		return err
	}

	if descheduler.Spec.DeschedulingIntervalSeconds == nil {
		return fmt.Errorf("descheduler should have an interval set")
	}

	if err := validateStrategies(descheduler.Spec.Strategies); err != nil {
		return err
	}
	if _, _, err := c.manageConfigMap(descheduler); err != nil {
		return err
	}
	if _, _, err := c.manageDeployment(descheduler); err != nil {
		return err
	}

	return nil
}

func validateStrategies(strategies []deschedulerv1beta1.Strategy) error {
	if len(strategies) == 0 {
		return fmt.Errorf("descheduler should have atleast one strategy enabled and it should be one of %v", strings.Join(validStrategies.List(), ","))
	}

	if len(strategies) > len(validStrategies) {
		return fmt.Errorf("descheduler can have a maximum of %v strategies enabled at this point of time", len(validStrategies))
	}

	invalidStrategies := make([]string, 0, len(strategies))
	for _, strategy := range strategies {
		if !validStrategies.Has(strings.ToLower(strategy.Name)) {
			invalidStrategies = append(invalidStrategies, strategy.Name)
		}
	}
	if len(invalidStrategies) > 0 {
		return fmt.Errorf("expected one of the %v to be enabled but found following invalid strategies %v",
			strings.Join(validStrategies.List(), ","), strings.Join(invalidStrategies, ","))
	}
	return nil
}

func (c *TargetConfigReconciler) manageConfigMap(descheduler *deschedulerv1beta1.KubeDescheduler) (*v1.ConfigMap, bool, error) {
	required := resourceread.ReadConfigMapV1OrDie(v410_00_assets.MustAsset("v4.1.0/kube-descheduler/configmap.yaml"))
	required.Name = descheduler.Name
	required.Namespace = descheduler.Namespace
	required.OwnerReferences = []metav1.OwnerReference{
		{
			APIVersion: "v1beta1",
			Kind:       "KubeDescheduler",
			Name:       descheduler.Name,
			UID:        descheduler.UID,
		},
	}
	configMapString, err := generateConfigMapString(descheduler.Spec.Strategies)
	if err != nil {
		return nil, false, err
	}
	required.Data = map[string]string{"policy.yaml": configMapString}
	return resourceapply.ApplyConfigMap(c.kubeClient.CoreV1(), c.eventRecorder, required)
}

// generateConfigMapString generates configmap needed for the string.
func generateConfigMapString(requestedStrategies []deschedulerv1beta1.Strategy) (string, error) {
	policy := &deschedulerapi.DeschedulerPolicy{Strategies: make(deschedulerapi.StrategyList)}
	// There is no need to do validation here. By the time, we reach here, validation would have already happened.
	for _, strategy := range requestedStrategies {
		switch strings.ToLower(strategy.Name) {
		case "duplicates":
			policy.Strategies["RemoveDuplicates"] = deschedulerapi.DeschedulerStrategy{Enabled: true}
		case "interpodantiaffinity":
			policy.Strategies["RemovePodsViolatingInterPodAntiAffinity"] = deschedulerapi.DeschedulerStrategy{Enabled: true}
		case "lownodeutilization":
			utilizationThresholds := deschedulerapi.NodeResourceUtilizationThresholds{NumberOfNodes: 0}
			thresholds := deschedulerapi.ResourceThresholds{}
			targetThresholds := deschedulerapi.ResourceThresholds{}
			for _, param := range strategy.Params {
				value, err := strconv.Atoi(param.Value)
				if err != nil {
					return "", err
				}
				switch param.Name {
				case "cputhreshold":
					thresholds[v1.ResourceCPU] = deschedulerapi.Percentage(value)
				case "memorythreshold":
					thresholds[v1.ResourceMemory] = deschedulerapi.Percentage(value)
				case "podsthreshold":
					thresholds[v1.ResourcePods] = deschedulerapi.Percentage(value)
				case "cputargetthreshold":
					targetThresholds[v1.ResourceCPU] = deschedulerapi.Percentage(value)
				case "memorytargetthreshold":
					targetThresholds[v1.ResourceMemory] = deschedulerapi.Percentage(value)
				case "podstargetthreshold":
					targetThresholds[v1.ResourcePods] = deschedulerapi.Percentage(value)
				case "nodes":
					utilizationThresholds.NumberOfNodes = value
				}
			}
			if len(thresholds) > 0 {
				utilizationThresholds.Thresholds = thresholds
			}
			if len(targetThresholds) > 0 {
				utilizationThresholds.TargetThresholds = thresholds
			}
			policy.Strategies["LowNodeUtilization"] = deschedulerapi.DeschedulerStrategy{Enabled: true,
				Params: deschedulerapi.StrategyParameters{
					NodeResourceUtilizationThresholds: utilizationThresholds,
				},
			}
		case "nodeaffinity":
			policy.Strategies["RemovePodsViolatingNodeAffinity"] = deschedulerapi.DeschedulerStrategy{Enabled: true,
				Params: deschedulerapi.StrategyParameters{
					NodeAffinityType: []string{"requiredDuringSchedulingIgnoredDuringExecution"},
				},
			}
		default:
			klog.Warningf("not using unknown strategy '%s'", strategy.Name)
		}
	}
	policyBytes, err := yaml.Marshal(policy)
	if err != nil {
		return "", err
	}
	return string(policyBytes), nil
}

func (c *TargetConfigReconciler) manageDeployment(descheduler *deschedulerv1beta1.KubeDescheduler) (*appsv1.Deployment, bool, error) {
	required := resourceread.ReadDeploymentV1OrDie(v410_00_assets.MustAsset("v4.1.0/kube-descheduler/deployment.yaml"))
	required.Name = descheduler.Name
	required.Namespace = descheduler.Namespace
	required.OwnerReferences = []metav1.OwnerReference{
		{
			APIVersion: "v1beta1",
			Kind:       "KubeDescheduler",
			Name:       descheduler.Name,
			UID:        descheduler.UID,
		},
	}
	required.Spec.Template.Spec.Containers[0].Image = descheduler.Spec.Image
	required.Spec.Template.Spec.Containers[0].Args = append(required.Spec.Template.Spec.Containers[0].Args,
		fmt.Sprintf("--descheduling-interval=%ss", strconv.Itoa(int(*descheduler.Spec.DeschedulingIntervalSeconds))))

	// Add any additional flags that were specified
	if len(descheduler.Spec.Flags) > 0 {
		required.Spec.Template.Spec.Containers[0].Args = append(required.Spec.Template.Spec.Containers[0].Args, descheduler.Spec.Flags...)
	}
	required.Spec.Template.Spec.Volumes[0].VolumeSource.ConfigMap.LocalObjectReference.Name = descheduler.Name
	return resourceapply.ApplyDeployment(c.kubeClient.AppsV1(), c.eventRecorder, required, 0, true)
}

// Run starts the kube-scheduler and blocks until stopCh is closed.
func (c *TargetConfigReconciler) Run(workers int, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	klog.Infof("Starting TargetConfigReconciler")
	defer klog.Infof("Shutting down TargetConfigReconciler")

	// doesn't matter what workers say, only start one.
	go wait.Until(c.runWorker, time.Second, stopCh)

	<-stopCh
}

func (c *TargetConfigReconciler) runWorker() {
	for c.processNextWorkItem() {
	}
}

func (c *TargetConfigReconciler) processNextWorkItem() bool {
	dsKey, quit := c.queue.Get()
	if quit {
		return false
	}
	defer c.queue.Done(dsKey)

	err := c.sync()
	if err == nil {
		c.queue.Forget(dsKey)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("%v failed with : %v", dsKey, err))
	c.queue.AddRateLimited(dsKey)

	return true
}

// eventHandler queues the operator to check spec and status
func (c *TargetConfigReconciler) eventHandler() cache.ResourceEventHandler {
	return cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { c.queue.Add(workQueueKey) },
		UpdateFunc: func(old, new interface{}) { c.queue.Add(workQueueKey) },
		DeleteFunc: func(obj interface{}) { c.queue.Add(workQueueKey) },
	}
}
