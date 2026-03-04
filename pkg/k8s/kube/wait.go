package kube

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Container waiting reasons (set by the kubelet as string literals).
const (
	ReasonContainerCreating          = "ContainerCreating"
	ReasonPodInitializing            = "PodInitializing"
	ReasonCrashLoopBackOff           = "CrashLoopBackOff"
	ReasonImagePullBackOff           = "ImagePullBackOff"
	ReasonErrImagePull               = "ErrImagePull"
	ReasonCreateContainerConfigError = "CreateContainerConfigError"
	ReasonInvalidImageName           = "InvalidImageName"
	ReasonCreateContainerError       = "CreateContainerError"
	ReasonRunContainerError          = "RunContainerError"
)

// Container terminated reasons (set by the kubelet as string literals).
const (
	ReasonOOMKilled          = "OOMKilled"
	ReasonError              = "Error"
	ReasonCompleted          = "Completed"
	ReasonContainerCannotRun = "ContainerCannotRun"
	ReasonDeadlineExceeded   = "DeadlineExceeded"
	ReasonEvicted            = "Evicted"
	ReasonStartError         = "StartError"
)

// reasonHints maps container state reasons to human-readable hints.
var reasonHints = map[string]string{
	ReasonCrashLoopBackOff:           "container keeps crashing and restarting — check application logs",
	ReasonImagePullBackOff:           "unable to pull the container image — verify the image name and registry credentials",
	ReasonErrImagePull:               "failed to pull the container image — verify the image exists and is accessible",
	ReasonCreateContainerConfigError: "invalid container configuration — check environment variable references and config mounts",
	ReasonInvalidImageName:           "the container image name is malformed",
	ReasonCreateContainerError:       "failed to create the container — check the pod spec and node resources",
	ReasonRunContainerError:          "the container failed to start — check the entrypoint/command",
	ReasonOOMKilled:                  "container was killed for exceeding its memory limit — increase the memory resource limit",
	ReasonContainerCannotRun:         "the container runtime refused to start the container",
	ReasonDeadlineExceeded:           "the pod exceeded its active deadline",
	ReasonEvicted:                    "the pod was evicted from the node — check node resource pressure",
	ReasonStartError:                 "the container failed during startup",
}

// terminalReasons are container waiting/terminated reasons that indicate the
// pod will never become ready without intervention.
var terminalReasons = map[string]bool{
	ReasonCrashLoopBackOff:           true,
	ReasonImagePullBackOff:           true,
	ReasonErrImagePull:               true,
	ReasonCreateContainerConfigError: true,
	ReasonInvalidImageName:           true,
	ReasonRunContainerError:          true,
}

// WaitForPod polls until a pod matching the label selector in the given
// namespace is running with all containers ready. It returns early with an
// error if a terminal failure is detected (e.g., CrashLoopBackOff,
// ImagePullBackOff, CreateContainerConfigError). On timeout it returns the
// best error it can glean from container statuses.
func WaitForPod(ctx context.Context, client kubernetes.Interface, ns, labelSelector string, timeout time.Duration) (*corev1.Pod, error) {
	deadline := time.After(timeout)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	var lastPodStatus string

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline:
			msg := fmt.Sprintf("timed out waiting for pod in %s (selector: %s)", ns, labelSelector)
			if lastPodStatus != "" {
				msg += ": " + lastPodStatus
			}
			return nil, fmt.Errorf("%s", msg)
		case <-ticker.C:
			pods, err := client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
				LabelSelector: labelSelector,
			})
			if err != nil {
				continue
			}

			for _, pod := range pods.Items {
				if podReady(&pod) {
					return &pod, nil
				}
				if reason := podTerminalReason(&pod); reason != "" {
					return nil, fmt.Errorf("pod %s in %s has terminal failure: %s", pod.Name, ns, podError(&pod))
				}
				if errMsg := podError(&pod); errMsg != "" {
					lastPodStatus = errMsg
				}
			}
		}
	}
}

// podTerminalReason returns the first terminal failure reason found on a pod,
// or empty string if the pod might still recover.
func podTerminalReason(pod *corev1.Pod) string {
	for _, cs := range append(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses...) {
		if cs.State.Waiting != nil && terminalReasons[cs.State.Waiting.Reason] {
			return cs.State.Waiting.Reason
		}
		if cs.State.Terminated != nil && terminalReasons[cs.State.Terminated.Reason] {
			return cs.State.Terminated.Reason
		}
	}
	return ""
}

// podReady returns true if all containers in the pod are ready and it is
// not being deleted (a terminating pod may briefly appear Running+Ready).
func podReady(pod *corev1.Pod) bool {
	if pod.DeletionTimestamp != nil {
		return false
	}
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if !cs.Ready {
			return false
		}
	}
	return len(pod.Status.ContainerStatuses) > 0
}

// podError extracts the most useful error string from a pod, including the pod
// phase and container-level failure details with human-readable hints.
func podError(pod *corev1.Pod) string {
	var parts []string

	phase := string(pod.Status.Phase)
	if phase != "" && phase != string(corev1.PodRunning) {
		detail := fmt.Sprintf("phase %s", phase)
		if pod.Status.Reason != "" {
			detail += ": " + pod.Status.Reason
		}
		if pod.Status.Message != "" {
			detail += " (" + pod.Status.Message + ")"
		}
		parts = append(parts, detail)
	}

	for _, cs := range append(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses...) {
		if cs.State.Waiting != nil {
			if detail := waitingError(cs.Name, cs.State.Waiting); detail != "" {
				parts = append(parts, detail)
			}
		}
		if cs.State.Terminated != nil {
			parts = append(parts, terminatedError(cs.Name, cs.State.Terminated))
		}
	}

	return strings.Join(parts, "; ")
}

func waitingError(name string, w *corev1.ContainerStateWaiting) string {
	if w.Reason == "" || w.Reason == ReasonContainerCreating || w.Reason == ReasonPodInitializing {
		return ""
	}
	detail := fmt.Sprintf("container %q: %s", name, w.Reason)
	if w.Message != "" {
		detail += " — " + w.Message
	}
	if hint, ok := reasonHints[w.Reason]; ok {
		detail += " (" + hint + ")"
	}
	return detail
}

func terminatedError(name string, t *corev1.ContainerStateTerminated) string {
	detail := fmt.Sprintf("container %q: %s (exit code %d)", name, t.Reason, t.ExitCode)
	if t.Message != "" {
		detail += " — " + t.Message
	}
	if hint, ok := reasonHints[t.Reason]; ok {
		detail += " (" + hint + ")"
	}
	return detail
}
