/*
Copyright 2022 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package actuation

import (
	"context"
	"fmt"
	"sort"
	"time"

	apiv1 "k8s.io/api/core/v1"
	policyv1beta1 "k8s.io/api/policy/v1beta1"
	kube_errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/autoscaler/cluster-autoscaler/metrics"
	"k8s.io/autoscaler/cluster-autoscaler/simulator/fake"
	"k8s.io/autoscaler/cluster-autoscaler/simulator/framework"
	"k8s.io/klog/v2"
	kubelet_config "k8s.io/kubernetes/pkg/kubelet/apis/config"

	acontext "k8s.io/autoscaler/cluster-autoscaler/context"
	"k8s.io/autoscaler/cluster-autoscaler/core/scaledown/status"
	"k8s.io/autoscaler/cluster-autoscaler/utils/daemonset"
	"k8s.io/autoscaler/cluster-autoscaler/utils/errors"
	pod_util "k8s.io/autoscaler/cluster-autoscaler/utils/pod"
)

const (
	// DefaultEvictionRetryTime is the time after CA retries failed pod eviction.
	DefaultEvictionRetryTime = 10 * time.Second
	// DefaultPodEvictionHeadroom is the extra time we wait to catch situations when the pod is ignoring SIGTERM and
	// is killed with SIGKILL after GracePeriodSeconds elapses
	DefaultPodEvictionHeadroom = 30 * time.Second
)

type evictionRegister interface {
	RegisterEviction(*apiv1.Pod)
}

// Evictor keeps configurations of pod eviction
type Evictor struct {
	EvictionRetryTime                time.Duration
	PodEvictionHeadroom              time.Duration
	evictionRegister                 evictionRegister
	shutdownGracePeriodByPodPriority []kubelet_config.ShutdownGracePeriodByPodPriority
	fullDsEviction                   bool
}

// NewEvictor returns an instance of Evictor.
func NewEvictor(evictionRegister evictionRegister, shutdownGracePeriodByPodPriority []kubelet_config.ShutdownGracePeriodByPodPriority, fullDsEviction bool) Evictor {
	sort.Slice(shutdownGracePeriodByPodPriority, func(i, j int) bool {
		return shutdownGracePeriodByPodPriority[i].Priority < shutdownGracePeriodByPodPriority[j].Priority
	})

	return Evictor{
		EvictionRetryTime:                DefaultEvictionRetryTime,
		PodEvictionHeadroom:              DefaultPodEvictionHeadroom,
		evictionRegister:                 evictionRegister,
		shutdownGracePeriodByPodPriority: shutdownGracePeriodByPodPriority,
		fullDsEviction:                   fullDsEviction,
	}
}

// DrainNode groups pods in the node in to priority groups and, evicts pods in the ascending order of priorities.
// If priority evictor is not enable, eviction of daemonSet pods is the best effort.
func (e Evictor) DrainNode(ctx *acontext.AutoscalingContext, nodeInfo *framework.NodeInfo) (map[string]status.PodEvictionResult, error) {
	return e.drainNode(ctx, nodeInfo, false)
}

// drainNodeForce performs similar logic to DrainNode, but forcefully deletes pods on drain failure.
func (e Evictor) drainNodeForce(ctx *acontext.AutoscalingContext, nodeInfo *framework.NodeInfo) (map[string]status.PodEvictionResult, error) {
	return e.drainNode(ctx, nodeInfo, true)
}

// drainNode implements the shared logic for draining a node, with the 'force' parameter
// determining whether to forcefully delete pods upon eviction failure.
func (e Evictor) drainNode(ctx *acontext.AutoscalingContext, nodeInfo *framework.NodeInfo, force bool) (map[string]status.PodEvictionResult, error) {
	node := nodeInfo.Node()
	dsPods, pods := podsToEvict(nodeInfo, ctx.DaemonSetEvictionForOccupiedNodes)
	if e.fullDsEviction {
		return e.drainNodeWithPodsBasedOnPodPriority(ctx, node, append(pods, dsPods...), nil, force)
	}
	return e.drainNodeWithPodsBasedOnPodPriority(ctx, node, pods, dsPods, force)
}

// EvictDaemonSetPods creates eviction objects for all DaemonSet pods on the node.
// Eviction of DaemonSet pods are best effort. Does not wait for evictions to finish.
func (e Evictor) EvictDaemonSetPods(ctx *acontext.AutoscalingContext, nodeInfo *framework.NodeInfo) (map[string]status.PodEvictionResult, error) {
	node := nodeInfo.Node()
	dsPods, _ := podsToEvict(nodeInfo, ctx.DaemonSetEvictionForEmptyNodes)
	return e.drainNodeWithPodsBasedOnPodPriority(ctx, node, nil, dsPods, false) // force option applies only to full eviction pods
}

// drainNodeWithPodsBasedOnPodPriority performs drain logic on the node based on pod priorities.
// Removes all pods, giving each pod group up to ShutdownGracePeriodSeconds to finish. The list of pods to evict has to be provided.
func (e Evictor) drainNodeWithPodsBasedOnPodPriority(ctx *acontext.AutoscalingContext, node *apiv1.Node, fullEvictionPods, bestEffortEvictionPods []*apiv1.Pod, force bool) (map[string]status.PodEvictionResult, error) {
	evictionResults := make(map[string]status.PodEvictionResult)

	groups := groupByPriority(e.shutdownGracePeriodByPodPriority, fullEvictionPods, bestEffortEvictionPods)
	for _, group := range groups {
		for _, pod := range group.FullEvictionPods {
			evictionResults[pod.Name] = status.PodEvictionResult{Pod: pod, TimedOut: false,
				Err: errors.NewAutoscalerErrorf(errors.UnexpectedScaleDownStateError, "Eviction did not attempted for the pod %s because some of the previous evictions failed", pod.Name)}
		}
	}

	for _, group := range groups {
		// If there are no pods in a particular range,
		// then do not wait for pods in that priority range.
		if len(group.FullEvictionPods) == 0 && len(group.BestEffortEvictionPods) == 0 {
			continue
		}

		var err error
		evictionResults, err = e.initiateEviction(ctx, node, group.FullEvictionPods, group.BestEffortEvictionPods, evictionResults, group.ShutdownGracePeriodSeconds, force)
		if err != nil {
			return evictionResults, err
		}

		// Evictions created successfully, wait ShutdownGracePeriodSeconds + podEvictionHeadroom to see if fullEviction pods really disappeared.
		evictionResults, err = e.waitPodsToDisappear(ctx, node, group.FullEvictionPods, evictionResults, group.ShutdownGracePeriodSeconds)
		if err != nil {
			return evictionResults, err
		}
	}
	klog.V(1).Infof("All pods removed from %s", node.Name)
	return evictionResults, nil
}

func (e Evictor) waitPodsToDisappear(ctx *acontext.AutoscalingContext, node *apiv1.Node, pods []*apiv1.Pod, evictionResults map[string]status.PodEvictionResult,
	maxTermination int64) (map[string]status.PodEvictionResult, error) {
	var allGone bool
	for start := time.Now(); time.Now().Sub(start) < time.Duration(maxTermination)*time.Second+e.PodEvictionHeadroom; time.Sleep(5 * time.Second) {
		allGone = true
		for _, pod := range pods {
			podReturned, err := ctx.ClientSet.CoreV1().Pods(pod.Namespace).Get(context.TODO(), pod.Name, metav1.GetOptions{})
			if err == nil && (podReturned == nil || podReturned.Spec.NodeName == node.Name) {
				klog.V(1).Infof("Not deleted yet %s/%s", pod.Namespace, pod.Name)
				allGone = false
				break
			}
			if err != nil && !kube_errors.IsNotFound(err) {
				klog.Errorf("Failed to check pod %s/%s: %v", pod.Namespace, pod.Name, err)
				allGone = false
				break
			}
		}
		if allGone {
			return evictionResults, nil
		}
	}

	for _, pod := range pods {
		podReturned, err := ctx.ClientSet.CoreV1().Pods(pod.Namespace).Get(context.TODO(), pod.Name, metav1.GetOptions{})
		if err == nil && (podReturned == nil || podReturned.Name == "" || podReturned.Spec.NodeName == node.Name) {
			evictionResults[pod.Name] = status.PodEvictionResult{Pod: pod, TimedOut: true, Err: nil}
		} else if err != nil && !kube_errors.IsNotFound(err) {
			evictionResults[pod.Name] = status.PodEvictionResult{Pod: pod, TimedOut: true, Err: err}
		} else {
			evictionResults[pod.Name] = status.PodEvictionResult{Pod: pod, TimedOut: false, Err: nil}
		}
	}

	return evictionResults, errors.NewAutoscalerErrorf(errors.TransientError, "Failed to drain node %s/%s: pods remaining after timeout", node.Namespace, node.Name)
}

func (e Evictor) initiateEviction(ctx *acontext.AutoscalingContext, node *apiv1.Node, fullEvictionPods, bestEffortEvictionPods []*apiv1.Pod, evictionResults map[string]status.PodEvictionResult,
	maxTermination int64, force bool) (map[string]status.PodEvictionResult, error) {

	retryUntil := time.Now().Add(ctx.MaxPodEvictionTime)
	fullEvictionConfirmations := make(chan status.PodEvictionResult, len(fullEvictionPods))
	bestEffortEvictionConfirmations := make(chan status.PodEvictionResult, len(bestEffortEvictionPods))

	for _, pod := range fullEvictionPods {
		evictionResults[pod.Name] = status.PodEvictionResult{Pod: pod, TimedOut: true, Err: nil}
		go func(pod *apiv1.Pod) {
			fullEvictionConfirmations <- e.evictPod(ctx, pod, retryUntil, maxTermination, true, force)
		}(pod)
	}

	for _, pod := range bestEffortEvictionPods {
		go func(pod *apiv1.Pod) {
			bestEffortEvictionConfirmations <- e.evictPod(ctx, pod, retryUntil, maxTermination, false, false) // force option applies only to full eviction pods
		}(pod)
	}

	for i := 0; i < len(fullEvictionPods)+len(bestEffortEvictionPods); i++ {
		select {
		case evictionResult := <-fullEvictionConfirmations:
			evictionResults[evictionResult.Pod.Name] = evictionResult
			if evictionResult.WasEvictionSuccessful() {
				metrics.RegisterEvictions(1, metrics.PodEvictionSucceed)
			} else {
				metrics.RegisterEvictions(1, metrics.PodEvictionFailed)
			}
		case <-bestEffortEvictionConfirmations:
		}
	}

	evictionErrs := make([]error, 0)
	for _, pod := range fullEvictionPods {
		result := evictionResults[pod.Name]
		if !result.WasEvictionSuccessful() {
			evictionErrs = append(evictionErrs, result.Err)
		}
	}
	if len(evictionErrs) != 0 {
		return evictionResults, errors.NewAutoscalerErrorf(errors.ApiCallError, "Failed to drain node %s/%s, due to following errors: %v", node.Namespace, node.Name, evictionErrs)
	}
	return evictionResults, nil
}

func (e Evictor) evictPod(ctx *acontext.AutoscalingContext, podToEvict *apiv1.Pod, retryUntil time.Time, maxTermination int64, fullEvictionPod bool, force bool) status.PodEvictionResult {
	ctx.Recorder.Eventf(podToEvict, apiv1.EventTypeNormal, "ScaleDown", "deleting pod for node scale down")

	termination := int64(apiv1.DefaultTerminationGracePeriodSeconds)
	if podToEvict.Spec.TerminationGracePeriodSeconds != nil {
		termination = *podToEvict.Spec.TerminationGracePeriodSeconds
	}
	if maxTermination > 0 && termination > maxTermination {
		termination = maxTermination
	}

	var lastError error
	for first := true; first || time.Now().Before(retryUntil); time.Sleep(e.EvictionRetryTime) {
		first = false
		eviction := &policyv1beta1.Eviction{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: podToEvict.Namespace,
				Name:      podToEvict.Name,
			},
			DeleteOptions: &metav1.DeleteOptions{
				GracePeriodSeconds: &termination,
			},
		}
		lastError = ctx.ClientSet.CoreV1().Pods(podToEvict.Namespace).Evict(context.TODO(), eviction)
		if lastError == nil || kube_errors.IsNotFound(lastError) {
			if e.evictionRegister != nil {
				e.evictionRegister.RegisterEviction(podToEvict)
			}
			return status.PodEvictionResult{Pod: podToEvict, TimedOut: false, Err: nil}
		}
	}

	klog.Errorf("Failed to evict pod %s, error: %v", podToEvict.Name, lastError)
	if force {
		// If eviction failed, forcefully delete the pod
		if err := forceDeletePod(ctx, podToEvict); err != nil {
			return status.PodEvictionResult{Pod: podToEvict, TimedOut: false, Err: err}
		}
		if e.evictionRegister != nil {
			e.evictionRegister.RegisterEviction(podToEvict)
		}
		return status.PodEvictionResult{Pod: podToEvict, TimedOut: false, Err: nil}
	}
	if fullEvictionPod {
		ctx.Recorder.Eventf(podToEvict, apiv1.EventTypeWarning, "ScaleDownFailed", "failed to delete pod for ScaleDown")
	}
	return status.PodEvictionResult{Pod: podToEvict, TimedOut: true, Err: fmt.Errorf("failed to evict pod %s/%s within allowed timeout (last error: %v)", podToEvict.Namespace, podToEvict.Name, lastError)}
}

func podsToEvict(nodeInfo *framework.NodeInfo, evictDsByDefault bool) (dsPods, nonDsPods []*apiv1.Pod) {
	for _, podInfo := range nodeInfo.Pods() {
		if pod_util.IsMirrorPod(podInfo.Pod) {
			continue
		} else if fake.IsFake(podInfo.Pod) {
			continue
		} else if pod_util.IsDaemonSetPod(podInfo.Pod) {
			dsPods = append(dsPods, podInfo.Pod)
		} else {
			nonDsPods = append(nonDsPods, podInfo.Pod)
		}
	}
	dsPodsToEvict := daemonset.PodsToEvict(dsPods, evictDsByDefault)
	return dsPodsToEvict, nonDsPods
}

func forceDeletePod(ctx *acontext.AutoscalingContext, pod *apiv1.Pod) error {
	klog.Infof("Starting force deletion of pod %s", pod.Name)
	if err := ctx.ClientSet.CoreV1().Pods(pod.Namespace).Delete(context.TODO(), pod.Name, metav1.DeleteOptions{}); err != nil {
		klog.Errorf("Failed to forcefully delete pod %s, error: %v", pod.Name, err)
		ctx.Recorder.Eventf(pod, apiv1.EventTypeWarning, "ScaleDownFailed", "failed to forcefully delete pod for ScaleDown")
		return fmt.Errorf("failed to forcefully delete unevicted pod %s/%s (last error: %v)", pod.Namespace, pod.Name, err)
	}
	return nil
}

type podEvictionGroup struct {
	kubelet_config.ShutdownGracePeriodByPodPriority
	FullEvictionPods       []*apiv1.Pod
	BestEffortEvictionPods []*apiv1.Pod
}
