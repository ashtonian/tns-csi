package driver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
)

// apiCallTimeout bounds each Kubernetes API call the recovery path makes, so a
// slow/unreachable API server cannot stall a reconciler loop until shutdown.
const apiCallTimeout = 10 * time.Second

// errNoKubeClient indicates the node plugin is not running in a cluster (or the
// client could not be built). Recovery treats this as "best effort, skip".
var errNoKubeClient = errors.New("recovery: in-cluster Kubernetes client unavailable")

// nodeKubeClient is the node plugin's window into the Kubernetes API for
// recovery: resolving a volume's PVC, emitting Events, and evicting/deleting
// pods. It is created only when recovery is enabled and initializes its
// clientset lazily so unit tests and out-of-cluster runs do not require an API
// server. All operations are best-effort: a missing client never blocks the
// CSI data path.
type nodeKubeClient struct {
	client  kubernetes.Interface
	initErr error
	nodeID  string
	once    sync.Once
}

func newNodeKubeClient(nodeID string) *nodeKubeClient {
	return &nodeKubeClient{nodeID: nodeID}
}

func (k *nodeKubeClient) clientset() (kubernetes.Interface, error) {
	k.once.Do(func() {
		config, err := rest.InClusterConfig()
		if err != nil {
			k.initErr = fmt.Errorf("%w: %w", errNoKubeClient, err)
			return
		}
		cs, err := kubernetes.NewForConfig(config)
		if err != nil {
			k.initErr = fmt.Errorf("%w: %w", errNoKubeClient, err)
			return
		}
		k.client = cs
	})
	return k.client, k.initErr
}

// pvcRef identifies a PersistentVolumeClaim.
type pvcRef struct {
	Namespace string
	Name      string
	PodUID    string // optional; set when resolved from a pod mount
}

func (r pvcRef) empty() bool { return r.Namespace == "" || r.Name == "" }

// ResolvePVCForVolume finds the PVC bound to the PV whose CSI volumeHandle equals
// volumeID. Returns an empty ref (no error) when it cannot be resolved, so
// callers can still proceed with logging-only behavior.
func (k *nodeKubeClient) ResolvePVCForVolume(ctx context.Context, volumeID string) pvcRef {
	if k == nil {
		return pvcRef{}
	}
	ctx, cancel := context.WithTimeout(ctx, apiCallTimeout)
	defer cancel()
	cs, err := k.clientset()
	if err != nil {
		klog.V(4).Infof("recovery: cannot resolve PVC for %s: %v", volumeID, err)
		return pvcRef{}
	}
	pvs, err := cs.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		klog.Warningf("recovery: failed to list PVs resolving %s: %v", volumeID, err)
		return pvcRef{}
	}
	for i := range pvs.Items {
		pv := &pvs.Items[i]
		if pv.Spec.CSI == nil || pv.Spec.CSI.VolumeHandle != volumeID {
			continue
		}
		if pv.Spec.ClaimRef == nil {
			return pvcRef{}
		}
		return pvcRef{Namespace: pv.Spec.ClaimRef.Namespace, Name: pv.Spec.ClaimRef.Name}
	}
	return pvcRef{}
}

// EmitEvent records a Kubernetes Event against the PVC. Best-effort: failures
// are logged at low verbosity and never propagated.
func (k *nodeKubeClient) EmitEvent(ctx context.Context, ref pvcRef, eventType, reason, message string) {
	if k == nil || ref.empty() {
		return
	}
	cs, err := k.clientset()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, apiCallTimeout)
	defer cancel()
	now := metav1.Now()
	ev := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "tns-csi-recovery-",
			Namespace:    ref.Namespace,
		},
		InvolvedObject: corev1.ObjectReference{
			Kind:      "PersistentVolumeClaim",
			Namespace: ref.Namespace,
			Name:      ref.Name,
		},
		Reason:         reason,
		Message:        message,
		Type:           eventType,
		Source:         corev1.EventSource{Component: "tns-csi-recovery", Host: k.nodeID},
		FirstTimestamp: now,
		LastTimestamp:  now,
		Count:          1,
	}
	_, createErr := cs.CoreV1().Events(ref.Namespace).Create(ctx, ev, metav1.CreateOptions{})
	if createErr != nil {
		klog.V(4).Infof("recovery: failed to emit Event on %s/%s: %v", ref.Namespace, ref.Name, createErr)
	}
}

// EvictPod removes a pod so its controller reschedules it. It honors the
// configured eviction mode: the Eviction API (which respects PodDisruptionBudgets)
// or a raw delete. Returns an error only when the removal genuinely failed.
func (k *nodeKubeClient) EvictPod(ctx context.Context, namespace, name, mode string) error {
	if k == nil {
		return errNoKubeClient
	}
	cs, err := k.clientset()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, apiCallTimeout)
	defer cancel()
	if mode == EvictModeDelete {
		return cs.CoreV1().Pods(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	}
	eviction := &policyv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
	}
	return cs.PolicyV1().Evictions(namespace).Evict(ctx, eviction)
}

// podRef identifies a pod the reconciler may act on.
type podRef struct {
	Namespace string
	Name      string
	UID       string
}

// podsOnNode lists pods scheduled to this node (a bounded field-selector query).
func (k *nodeKubeClient) podsOnNode(ctx context.Context) (*corev1.PodList, error) {
	cs, err := k.clientset()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, apiCallTimeout)
	defer cancel()
	return cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + k.nodeID,
	})
}

// isActionablePod reports whether a pod is a Running, non-terminating pod we may
// evict. Pods already terminating or not yet running are skipped.
func isActionablePod(pod *corev1.Pod) bool {
	if pod.DeletionTimestamp != nil {
		return false
	}
	return pod.Status.Phase == corev1.PodRunning
}

// PodsUsingPVC returns the actionable pods on this node that consume the named
// PVC. Used by the kmsg watcher to evict the consumer of a shut-down filesystem.
func (k *nodeKubeClient) PodsUsingPVC(ctx context.Context, ref pvcRef) []podRef {
	if k == nil || ref.empty() {
		return nil
	}
	pods, err := k.podsOnNode(ctx)
	if err != nil {
		klog.Warningf("recovery: failed to list pods for PVC %s/%s: %v", ref.Namespace, ref.Name, err)
		return nil
	}
	var out []podRef
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Namespace != ref.Namespace || !isActionablePod(pod) {
			continue
		}
		for vi := range pod.Spec.Volumes {
			pvc := pod.Spec.Volumes[vi].PersistentVolumeClaim
			if pvc != nil && pvc.ClaimName == ref.Name {
				out = append(out, podRef{Namespace: pod.Namespace, Name: pod.Name, UID: string(pod.UID)})
				break
			}
		}
	}
	return out
}

// PodByUID resolves a pod UID (extracted from a mount path) to a namespaced,
// actionable pod on this node. Used by the stale-bind healer.
func (k *nodeKubeClient) PodByUID(ctx context.Context, uid string) (podRef, bool) {
	if k == nil || uid == "" {
		return podRef{}, false
	}
	pods, err := k.podsOnNode(ctx)
	if err != nil {
		klog.Warningf("recovery: failed to list pods resolving uid %s: %v", uid, err)
		return podRef{}, false
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if string(pod.UID) == uid && isActionablePod(pod) {
			return podRef{Namespace: pod.Namespace, Name: pod.Name, UID: uid}, true
		}
	}
	return podRef{}, false
}

// RecoveryPaused reports whether the cluster kill switch is engaged: a ConfigMap
// named "tns-csi-recovery" in the plugin's namespace with data key
// "enabled" == "false". Absent ConfigMap/namespace = not paused (fail open to
// the configured mode). Best-effort; never blocks.
func (k *nodeKubeClient) RecoveryPaused(ctx context.Context) bool {
	if k == nil {
		return false
	}
	cs, err := k.clientset()
	if err != nil {
		return false
	}
	ns := inClusterNamespace()
	if ns == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, apiCallTimeout)
	defer cancel()
	cm, err := cs.CoreV1().ConfigMaps(ns).Get(ctx, recoveryConfigMapName, metav1.GetOptions{})
	if err != nil {
		return false
	}
	return cm.Data[recoveryConfigMapEnabledKey] == "false"
}

const (
	recoveryConfigMapName       = "tns-csi-recovery"
	recoveryConfigMapEnabledKey = "enabled"
)

// inClusterNamespace reads the pod's namespace from the standard service account
// mount. Returns "" when not running in a cluster.
func inClusterNamespace() string {
	data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
