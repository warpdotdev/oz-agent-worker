package worker

import (
	"context"
	"fmt"
	"sync"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/tools/cache"
)

const kubernetesJobUIDIndex = "controllingJobUID"

type kubernetesPodObservation struct {
	cache.SharedIndexInformer
	stop        context.CancelFunc
	done        chan struct{}
	users       int
	mu          sync.Mutex
	subscribers map[k8stypes.UID]map[chan struct{}]struct{}
}

// acquirePodObservation shares one watch while either execution or standby exit
// consumers are attached. A consumer's cancellation must not stop other consumers.
func (b *KubernetesBackend) acquirePodObservation(ctx context.Context) (*kubernetesPodObservation, func()) {
	b.podObservationMu.Lock()
	defer b.podObservationMu.Unlock()
	if b.podObservation == nil {
		informerCtx, stop := context.WithCancel(context.WithoutCancel(ctx))
		pods := &kubernetesPodObservation{
			SharedIndexInformer: b.newPodInformer(),
			stop:                stop,
			done:                make(chan struct{}),
			subscribers:         make(map[k8stypes.UID]map[chan struct{}]struct{}),
		}
		_, _ = pods.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc: pods.notify,
			UpdateFunc: func(old, current interface{}) {
				pods.notify(old)
				pods.notify(current)
			},
			DeleteFunc: pods.notify,
		})
		b.podObservation = pods
		go func() {
			defer close(pods.done)
			pods.Run(informerCtx.Done())
		}()
	}
	pods := b.podObservation
	pods.users++
	return pods, func() {
		b.podObservationMu.Lock()
		defer b.podObservationMu.Unlock()
		pods.users--
		if pods.users == 0 {
			pods.stop()
			<-pods.done
			b.podObservation = nil
		}
	}
}

func (b *KubernetesBackend) newPodInformer() cache.SharedIndexInformer {
	return coreinformers.NewFilteredPodInformer(b.clientset, b.config.Namespace, 0, cache.Indexers{
		kubernetesJobUIDIndex: func(obj interface{}) ([]string, error) {
			owner := metav1.GetControllerOf(obj.(*corev1.Pod))
			if owner == nil || owner.Kind != "Job" {
				return nil, nil
			}
			return []string{string(owner.UID)}, nil
		},
	}, func(options *metav1.ListOptions) {
		options.LabelSelector = fmt.Sprintf("%s=%s", kubernetesWorkerHashLabel, kubernetesLabelHash(b.config.WorkerID))
	})
}

func (p *kubernetesPodObservation) subscribe(uid k8stypes.UID) (<-chan struct{}, func()) {
	updates := make(chan struct{}, 1)
	p.mu.Lock()
	if p.subscribers[uid] == nil {
		p.subscribers[uid] = make(map[chan struct{}]struct{})
	}
	p.subscribers[uid][updates] = struct{}{}
	// Inspect the cache after subscribing, including changes delivered before
	// registration. Later notifications coalesce because consumers read the cache.
	updates <- struct{}{}
	p.mu.Unlock()
	return updates, func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		delete(p.subscribers[uid], updates)
		if len(p.subscribers[uid]) == 0 {
			delete(p.subscribers, uid)
		}
	}
}

func (p *kubernetesPodObservation) notify(obj interface{}) {
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tombstone.Obj
	}
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return
	}
	owner := metav1.GetControllerOf(pod)
	if owner == nil || owner.Kind != "Job" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for updates := range p.subscribers[owner.UID] {
		select {
		case updates <- struct{}{}:
		default:
		}
	}
}

func (p *kubernetesPodObservation) podsForJob(uid k8stypes.UID) []corev1.Pod {
	objects, _ := p.GetIndexer().ByIndex(kubernetesJobUIDIndex, string(uid))
	pods := make([]corev1.Pod, 0, len(objects))
	for _, obj := range objects {
		pods = append(pods, *obj.(*corev1.Pod).DeepCopy())
	}
	return pods
}
