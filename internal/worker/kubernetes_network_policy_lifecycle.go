package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/warpdotdev/oz-agent-worker/internal/log"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8stypes "k8s.io/apimachinery/pkg/types"
)

var networkLauncherIncarnation = uuid.NewString()

type networkPolicyConsumer struct {
	RunID string `json:"run_id"`
	ExecutionID string `json:"execution_id"`
	JobName string `json:"job_name"`
	Launcher string `json:"launcher"`
	JobUID k8stypes.UID `json:"job_uid,omitempty"`
}

type networkPolicyLedger struct {
	Version int `json:"version"`
	Consumers []networkPolicyConsumer `json:"consumers"`
}

func readNetworkLedger(object *unstructured.Unstructured) (networkPolicyLedger, error) {
	var ledger networkPolicyLedger
	raw := object.GetAnnotations()[networkConsumersAnnotation]
	if err := json.Unmarshal([]byte(raw), &ledger); err != nil {
		return ledger, fmt.Errorf("policy %s UID %s has a malformed consumer ledger: %w", object.GetName(), object.GetUID(), err)
	}
	if ledger.Version != 1 {
		return ledger, fmt.Errorf("policy %s UID %s has unsupported consumer ledger version %d", object.GetName(), object.GetUID(), ledger.Version)
	}
	seen := make(map[[2]string]bool)
	for _, entry := range ledger.Consumers {
		key := [2]string{entry.RunID, entry.ExecutionID}
		if entry.RunID == "" || entry.ExecutionID == "" || entry.Launcher == "" ||
			entry.JobName != kubernetesTaskJobName(entry.RunID, entry.ExecutionID) || seen[key] {
			return ledger, fmt.Errorf("policy %s UID %s has an invalid/duplicate consumer %q/%q", object.GetName(), object.GetUID(), entry.RunID, entry.ExecutionID)
		}
		seen[key] = true
	}
	return ledger, nil
}

func writeNetworkLedger(object *unstructured.Unstructured, ledger networkPolicyLedger) error {
	data, err := json.Marshal(ledger)
	if err != nil {
		return err
	}
	annotations := object.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations[networkConsumersAnnotation] = string(data)
	size := 0
	for key, value := range annotations {
		size += len(key)+len(value)
	}
	if size >= 256*1024 {
		return fmt.Errorf("network policy annotations exceed the 256 KiB limit; wait for consumers to stop")
	}
	object.SetAnnotations(annotations)
	return nil
}

func validateManagedNetworkPolicy(object *unstructured.Unstructured) (networkPolicyLedger, error) {
	id := object.GetLabels()[networkPolicyIDLabel]
	if object.GetUID() == "" || object.GetResourceVersion() == "" ||
		object.GetLabels()[networkManagedLabel] != "true" || len(object.GetOwnerReferences()) != 0 ||
		!slices.Contains(object.GetFinalizers(), networkPolicyFinalizer) {
		return networkPolicyLedger{}, fmt.Errorf("policy %s UID %s lacks managed identity, immutable ownership, or consumer finalizer", object.GetName(), object.GetUID())
	}
	canonical, err := canonicalNetworkSpec(object, id)
	if err != nil {
		return networkPolicyLedger{}, err
	}
	sum := sha256.Sum256(canonical)
	digest := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:]))
	if id != digest || object.GetName() != "warp-np-"+digest {
		return networkPolicyLedger{}, fmt.Errorf("policy %s UID %s live enforcement content does not match its fingerprint", object.GetName(), object.GetUID())
	}
	return readNetworkLedger(object)
}

func validateMatchingNetworkPolicy(object, intended *unstructured.Unstructured) (networkPolicyLedger, error) {
	ledger, err := validateManagedNetworkPolicy(object)
	if err != nil {
		return ledger, err
	}
	id := intended.GetLabels()[networkPolicyIDLabel]
	actual, err := canonicalNetworkSpec(object, id)
	if err != nil {
		return ledger, err
	}
	expected, err := canonicalNetworkSpec(intended, id)
	if err != nil {
		return ledger, err
	}
	if !bytes.Equal(actual, expected) {
		return ledger, fmt.Errorf("network policy %s conflicts with intended enforcement content", object.GetName())
	}
	return ledger, nil
}

func pauseNetworkRetry(ctx context.Context) error {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (p networkPolicyProvider) register(ctx context.Context, intended *unstructured.Unstructured, entry networkPolicyConsumer) (*unstructured.Unstructured, error) {
	intended.SetNamespace(p.backend.config.Namespace)
	if err := writeNetworkLedger(intended, networkPolicyLedger{Version: 1, Consumers: []networkPolicyConsumer{entry}}); err != nil {
		return nil, err
	}
	for ctx.Err() == nil {
		object, err := p.get(ctx, intended.GetName())
		insertedByCreate := false
		if apierrors.IsNotFound(err) {
			createErr := p.create(ctx, intended)
			// Recover the original create, including an accepted request whose response was lost.
			object, err = p.get(ctx, intended.GetName())
			if err != nil {
				return nil, fmt.Errorf("cannot establish policy creation result (%v): %w", createErr, err)
			}
			insertedByCreate = createErr == nil || (!apierrors.IsAlreadyExists(createErr) && isTransientKubernetesAPIError(createErr))
		}
		if err != nil {
			if isTransientKubernetesAPIError(err) && pauseNetworkRetry(ctx) == nil {
				continue
			}
			return nil, err
		}
		ledger, err := validateMatchingNetworkPolicy(object, intended)
		if err != nil {
			return nil, err
		}
		if insertedByCreate {
			for _, existing := range ledger.Consumers {
				if existing == entry {
					return object, nil
				}
			}
			return nil, fmt.Errorf("created policy lacks this launcher's exact reservation")
		}
		if object.GetDeletionTimestamp() != nil {
			if err := pauseNetworkRetry(ctx); err != nil {
				return nil, fmt.Errorf("policy %s UID %s is terminating; existing consumers must stop before reuse: %w", object.GetName(), object.GetUID(), err)
			}
			continue
		}
		for _, existing := range ledger.Consumers {
			if existing.RunID == entry.RunID && existing.ExecutionID == entry.ExecutionID {
				return nil, fmt.Errorf("execution %s/%s already has a durable launch registration; it cannot authorize another Job POST", entry.RunID, entry.ExecutionID)
			}
		}
		ledger.Consumers = append(ledger.Consumers, entry)
		if err := writeNetworkLedger(object, ledger); err != nil {
			return nil, err
		}
		err = p.patchMetadata(ctx, object)
		if err == nil {
			return object, nil
		}
		if !apierrors.IsConflict(err) && isTransientKubernetesAPIError(err) {
			recovered, readErr := p.get(ctx, intended.GetName())
			if readErr != nil {
				return nil, fmt.Errorf("cannot recover registration response: %w", readErr)
			}
			recoveredLedger, readErr := validateMatchingNetworkPolicy(recovered, intended)
			if readErr != nil || recovered.GetUID() != object.GetUID() {
				return nil, fmt.Errorf("policy changed during registration recovery: %v", readErr)
			}
			for _, existing := range recoveredLedger.Consumers {
				if existing == entry {
					return recovered, nil
				}
			}
			// An unresolved patch may still succeed; do not issue another launch.
			return nil, fmt.Errorf("registration outcome is ambiguous for %s/%s: %w", entry.RunID, entry.ExecutionID, err)
		}
		if !apierrors.IsConflict(err) {
			return nil, err
		}
		if err := pauseNetworkRetry(ctx); err != nil {
			return nil, err
		}
	}
	return nil, ctx.Err()
}

func networkExecutionMatches(meta metav1.Object, entry networkPolicyConsumer, policy *unstructured.Unstructured) bool {
	annotations, labels := meta.GetAnnotations(), meta.GetLabels()
	return annotations[networkRunAnnotation] == entry.RunID &&
		annotations[networkExecutionAnnotation] == entry.ExecutionID &&
		annotations[networkPolicyUIDAnnotation] == string(policy.GetUID()) &&
		labels[networkRestrictedLabel] == "true" &&
		labels[networkPolicyIDLabel] == policy.GetLabels()[networkPolicyIDLabel]
}

func (p networkPolicyProvider) bind(ctx context.Context, policy *unstructured.Unstructured, entry networkPolicyConsumer, jobUID k8stypes.UID) error {
	for ctx.Err() == nil {
		object, err := p.get(ctx, policy.GetName())
		if err != nil {
			return err
		}
		if object.GetUID() != policy.GetUID() {
			return fmt.Errorf("policy UID changed before Job binding")
		}
		ledger, err := validateMatchingNetworkPolicy(object, policy)
		if err != nil {
			return err
		}
		found := false
		for i, existing := range ledger.Consumers {
			if existing.RunID == entry.RunID && existing.ExecutionID == entry.ExecutionID {
				if existing.Launcher != entry.Launcher || (existing.JobUID != "" && existing.JobUID != jobUID) {
					return fmt.Errorf("consumer identity changed before Job binding")
				}
				found = true
				ledger.Consumers[i].JobUID = jobUID
			}
		}
		if !found || jobUID == "" {
			return fmt.Errorf("consumer registration or Job UID missing at binding")
		}
		if err := writeNetworkLedger(object, ledger); err != nil {
			return err
		}
		err = p.patchMetadata(ctx, object)
		if err == nil {
			return nil
		}
		if !apierrors.IsConflict(err) && !isTransientKubernetesAPIError(err) {
			return err
		}
		if err := pauseNetworkRetry(ctx); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func (b *KubernetesBackend) createRestrictedJob(ctx context.Context, params *TaskParams, job *batchv1.Job) (*batchv1.Job, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	key := [2]string{params.TaskID, taskExecutionID(params)}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		b.networkLaunchMu.Lock()
		pending, exists := b.networkLaunches[key]
		if !exists {
			if b.networkLaunches == nil {
				b.networkLaunches = make(map[[2]string]chan struct{})
			}
			pending = make(chan struct{})
			b.networkLaunches[key] = pending
			b.networkLaunchMu.Unlock()
			defer func() {
				b.networkLaunchMu.Lock()
				delete(b.networkLaunches, key)
				close(pending)
				b.networkLaunchMu.Unlock()
			}()
			break
		}
		b.networkLaunchMu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-pending:
		}
	}
	if !b.config.NetworkPolicy.Enabled {
		return nil, fmt.Errorf("backend.kubernetes.network_policy.enabled is false")
	}
	if err := validateNetworkPolicy(params.NetworkPolicy); err != nil {
		return nil, err
	}
	provider, err := b.discoverNetworkPolicyProvider()
	if err != nil {
		return nil, err
	}
	dns := b.config.NetworkPolicy.DNS
	needsDNS := false
	for _, rule := range params.NetworkPolicy.Rules {
		for _, host := range rule.Hosts {
			if _, err := netip.ParseAddr(host); err != nil {
				needsDNS = true
			}
		}
	}
	var resolver networkHostResolver
	if needsDNS {
		dns, err = b.networkPolicyDNS()
		if err != nil {
			return nil, err
		}
		resolver = networkDNSResolver(dns.ServerIPs)
	}
	intended, err := compileNetworkPolicy(ctx, params.NetworkPolicy, provider.cilium, dns, resolver)
	if err != nil {
		return nil, err
	}
	entry := networkPolicyConsumer{RunID: params.TaskID, ExecutionID: taskExecutionID(params), JobName: job.Name, Launcher: networkLauncherIncarnation}
	policy, err := provider.register(ctx, intended, entry)
	if err != nil {
		return nil, fmt.Errorf("register restricted execution %s/%s: %w", entry.RunID, entry.ExecutionID, err)
	}
	applyNetworkExecutionMetadata(job, entry, policy)
	applyNetworkExecutionMetadata(&job.Spec.Template, entry, policy)
	if err := ctx.Err(); err != nil {
		b.releaseRejectedNetworkLaunch(ctx, provider, policy, entry)
		return nil, err
	}
	// A timeout is not permission to repeat this POST or discard its reservation.
	created := &batchv1.Job{}
	createErr := b.clientset.BatchV1().RESTClient().Post().Namespace(b.config.Namespace).
		Resource("jobs").Body(job).MaxRetries(0).Do(ctx).Into(created)
	if createErr != nil {
		if apierrors.IsInvalid(createErr) || apierrors.IsBadRequest(createErr) || apierrors.IsForbidden(createErr) || apierrors.IsUnauthorized(createErr) {
			b.releaseRejectedNetworkLaunch(ctx, provider, policy, entry)
			return nil, fmt.Errorf("Job launch rejected before admission: %w", createErr)
		}
		created, err = b.getTaskJob(ctx, job.Name)
		if err != nil {
			return nil, fmt.Errorf("Job launch outcome unresolved; reservation retained (POST: %v; GET: %w)", createErr, err)
		}
	}
	if created == nil || created.Name != entry.JobName || !networkExecutionMatches(created, entry, policy) {
		return nil, fmt.Errorf("Job identity conflicts with restricted execution; reservation retained")
	}
	if !networkExecutionMatches(&created.Spec.Template, entry, policy) {
		stopCtx, stopCancel := context.WithTimeout(context.WithoutCancel(ctx), kubernetesCleanupTimeout)
		defer stopCancel()
		err := fmt.Errorf("Job Pod template conflicts with restricted execution")
		stopErr := b.requestJobExecutionStop(stopCtx, created, err)
		return nil, fmt.Errorf("%w (stop request: %v); reservation retained", err, stopErr)
	}
	if err := provider.bind(ctx, policy, entry, created.UID); err != nil {
		stopCtx, stopCancel := context.WithTimeout(context.WithoutCancel(ctx), kubernetesCleanupTimeout)
		defer stopCancel()
		stopErr := b.requestJobExecutionStop(stopCtx, created, err)
		return nil, fmt.Errorf("bind restricted Job UID: %w (stop request: %v); reservation retained", err, stopErr)
	}
	return created, nil
}

func applyNetworkExecutionMetadata(meta metav1.Object, entry networkPolicyConsumer, policy *unstructured.Unstructured) {
	labels := meta.GetLabels()
	if labels == nil {
		labels = make(map[string]string)
	}
	labels[networkRestrictedLabel] = "true"
	labels[networkPolicyIDLabel] = policy.GetLabels()[networkPolicyIDLabel]
	meta.SetLabels(labels)
	annotations := meta.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	for key := range annotations {
		if strings.HasPrefix(key, "warp-") {
			delete(annotations, key)
		}
	}
	annotations[networkRunAnnotation] = entry.RunID
	annotations[networkExecutionAnnotation] = entry.ExecutionID
	annotations[networkPolicyUIDAnnotation] = string(policy.GetUID())
	meta.SetAnnotations(annotations)
}

func (b *KubernetesBackend) releaseRejectedNetworkLaunch(ctx context.Context, provider networkPolicyProvider, policy *unstructured.Unstructured, entry networkPolicyConsumer) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), kubernetesCleanupTimeout)
	defer cancel()
	_, err := retryKubernetesAPI(cleanupCtx, kubernetesAPIBackoff(), func() (struct{}, error) {
		object, err := provider.get(cleanupCtx, policy.GetName())
		if err != nil {
			return struct{}{}, err
		}
		if object.GetUID() != policy.GetUID() {
			return struct{}{}, fmt.Errorf("policy UID changed while releasing a rejected launch")
		}
		ledger, err := validateMatchingNetworkPolicy(object, policy)
		if err != nil {
			return struct{}{}, err
		}
		ledger.Consumers = slices.DeleteFunc(ledger.Consumers, func(existing networkPolicyConsumer) bool { return existing == entry })
		if err := writeNetworkLedger(object, ledger); err != nil {
			return struct{}{}, err
		}
		return struct{}{}, provider.patchMetadata(cleanupCtx, object)
	})
	if err != nil && !apierrors.IsNotFound(err) {
		log.Errorf(ctx, "Retaining rejected launch %s/%s on policy UID %s after cleanup error: %v", entry.RunID, entry.ExecutionID, policy.GetUID(), err)
	}
	b.notifyNetworkPolicyReconciler()
}

func networkPodStopped(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
		return false
	}
	statuses := make(map[string]corev1.ContainerStatus)
	for _, status := range append(append(slices.Clone(pod.Status.ContainerStatuses), pod.Status.InitContainerStatuses...), pod.Status.EphemeralContainerStatuses...) {
		if status.State.Terminated == nil || status.State.Running != nil || status.State.Waiting != nil {
			return false
		}
		statuses[status.Name] = status
	}
	for _, container := range append(slices.Clone(pod.Spec.Containers), pod.Spec.InitContainers...) {
		if _, ok := statuses[container.Name]; !ok {
			return false
		}
	}
	for _, container := range pod.Spec.EphemeralContainers {
		if _, ok := statuses[container.Name]; !ok {
			return false
		}
	}
	return len(pod.Spec.Containers) > 0
}

func (b *KubernetesBackend) networkConsumerStopped(ctx context.Context, policy *unstructured.Unstructured, entry networkPolicyConsumer) (bool, error) {
	job, err := b.getTaskJob(ctx, entry.JobName)
	if err != nil && !apierrors.IsNotFound(err) {
		return false, err
	}
	if err == nil {
		if job.UID != entry.JobUID || !networkExecutionMatches(job, entry, policy) || !networkExecutionMatches(&job.Spec.Template, entry, policy) {
			return false, fmt.Errorf("Job %s conflicts with consumer %s/%s UID %s", entry.JobName, entry.RunID, entry.ExecutionID, entry.JobUID)
		}
		if !jobComplete(job) && !jobFailed(job) {
			return false, nil
		}
	}
	pods, err := b.clientset.CoreV1().Pods(b.config.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: batchv1.ControllerUidLabel+"="+string(entry.JobUID),
	})
	if err != nil {
		return false, err
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		owner := metav1.GetControllerOf(pod)
		if owner == nil || owner.UID != entry.JobUID || owner.Kind != "Job" ||
			!networkExecutionMatches(pod, entry, policy) || !networkPodStopped(pod) {
			return false, nil
		}
	}
	return true, nil
}

func (b *KubernetesBackend) networkMatchingObjectsStopped(ctx context.Context, policy *unstructured.Unstructured) (bool, error) {
	selector := networkRestrictedLabel+"=true,"+networkPolicyIDLabel+"="+policy.GetLabels()[networkPolicyIDLabel]
	jobs, err := b.clientset.BatchV1().Jobs(b.config.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return false, err
	}
	for i := range jobs.Items {
		job := &jobs.Items[i]
		entry := networkPolicyConsumer{RunID: job.Annotations[networkRunAnnotation], ExecutionID: job.Annotations[networkExecutionAnnotation]}
		priorPolicy := policy.DeepCopy()
		priorPolicy.SetUID(k8stypes.UID(job.Annotations[networkPolicyUIDAnnotation]))
		if entry.RunID == "" || entry.ExecutionID == "" || job.Name != kubernetesTaskJobName(entry.RunID, entry.ExecutionID) ||
			job.UID == "" || priorPolicy.GetUID() == "" ||
			!networkExecutionMatches(job, entry, priorPolicy) || !networkExecutionMatches(&job.Spec.Template, entry, priorPolicy) ||
			(!jobComplete(job) && !jobFailed(job)) {
			return false, fmt.Errorf("unexplained or active matching Job %s blocks policy %s UID %s cleanup", job.Name, policy.GetName(), policy.GetUID())
		}
	}
	pods, err := b.clientset.CoreV1().Pods(b.config.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return false, err
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		entry := networkPolicyConsumer{RunID: pod.Annotations[networkRunAnnotation], ExecutionID: pod.Annotations[networkExecutionAnnotation]}
		owner := metav1.GetControllerOf(pod)
		if entry.RunID == "" || entry.ExecutionID == "" || pod.Annotations[networkPolicyUIDAnnotation] == "" ||
			owner == nil || owner.Kind != "Job" || owner.UID == "" || owner.Name != kubernetesTaskJobName(entry.RunID, entry.ExecutionID) ||
			pod.Labels[batchv1.ControllerUidLabel] != string(owner.UID) ||
			!networkPodStopped(pod) {
			return false, fmt.Errorf("unexplained or active matching Pod %s blocks policy %s UID %s cleanup", pod.Name, policy.GetName(), policy.GetUID())
		}
	}
	return true, nil
}

func (p networkPolicyProvider) reconcile(ctx context.Context, observed *unstructured.Unstructured) error {
	for ctx.Err() == nil {
		object, err := p.get(ctx, observed.GetName())
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if object.GetUID() != observed.GetUID() {
			return fmt.Errorf("policy %s UID was replaced during reconciliation", observed.GetName())
		}
		ledger, err := validateManagedNetworkPolicy(object)
		if err != nil {
			return err
		}
		changed := false
		for i, entry := range ledger.Consumers {
			if entry.JobUID == "" {
				job, getErr := p.backend.getTaskJob(ctx, entry.JobName)
				if apierrors.IsNotFound(getErr) {
					continue
				}
				if getErr != nil {
					return getErr
				}
				if !networkExecutionMatches(job, entry, object) || !networkExecutionMatches(&job.Spec.Template, entry, object) || job.UID == "" {
					return fmt.Errorf("unbound consumer %s/%s conflicts with Job %s; reservation retained", entry.RunID, entry.ExecutionID, entry.JobName)
				}
				ledger.Consumers[i].JobUID = job.UID
				changed = true
				break
			}
			stopped, err := p.backend.networkConsumerStopped(ctx, object, entry)
			if err != nil {
				return err
			}
			if stopped {
				ledger.Consumers = slices.Delete(ledger.Consumers, i, i+1)
				changed = true
				break
			}
		}
		if changed {
			if err := writeNetworkLedger(object, ledger); err != nil {
				return err
			}
			err = p.patchMetadata(ctx, object)
		} else if len(ledger.Consumers) > 0 {
			return nil
		} else {
			stopped, scanErr := p.backend.networkMatchingObjectsStopped(ctx, object)
			if scanErr != nil {
				return scanErr
			}
			if !stopped {
				return nil
			}
			if object.GetDeletionTimestamp() == nil {
				err = p.delete(ctx, object)
			} else {
				// Terminating policies cannot accept registrations; recheck the ledger
				// and objects on this fresh read before removing only our finalizer.
				object.SetFinalizers(slices.DeleteFunc(object.GetFinalizers(), func(value string) bool { return value == networkPolicyFinalizer }))
				err = p.patchMetadata(ctx, object)
				if err == nil {
					return nil
				}
			}
		}
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil && !apierrors.IsConflict(err) && !isTransientKubernetesAPIError(err) {
			return err
		}
		if err != nil {
			if err := pauseNetworkRetry(ctx); err != nil {
				return err
			}
		}
	}
	return ctx.Err()
}

func (b *KubernetesBackend) reconcileNetworkPolicies(ctx context.Context) {
	for _, cilium := range []bool{false, true} {
		if cilium && b.networkDynamic == nil {
			continue
		}
		provider := networkPolicyProvider{backend: b, cilium: cilium}
		objects, err := provider.list(ctx)
		if cilium && apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			log.Errorf(ctx, "Cannot list managed network policies (cilium=%t); consumers retained: %v", cilium, err)
			continue
		}
		for i := range objects {
			object := &objects[i]
			_, err := retryKubernetesAPI(ctx, kubernetesAPIBackoff(), func() (struct{}, error) {
				return struct{}{}, provider.reconcile(ctx, object)
			})
			if err != nil {
				log.Errorf(ctx, "Network policy %s UID %s cleanup retained consumers %s: %v", object.GetName(), object.GetUID(), object.GetAnnotations()[networkConsumersAnnotation], err)
			}
		}
	}
}

func (b *KubernetesBackend) runNetworkPolicyReconciler(ctx context.Context) {
	ticker := time.NewTicker(30*time.Second)
	defer ticker.Stop()
	for {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), kubernetesCleanupTimeout)
		b.reconcileNetworkPolicies(cleanupCtx)
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-b.networkReconcile:
		}
	}
}

func (b *KubernetesBackend) notifyNetworkPolicyReconciler() {
	select {
	case b.networkReconcile <- struct{}{}:
	default:
	}
}
