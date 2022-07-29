package featureflags

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	informercorev1 "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

type FeatureList struct {
	ctx       context.Context
	logger    *zap.Logger
	features  []FeatureFlag
	clientset kubernetes.Interface
	queue     workqueue.RateLimitingInterface
	notify    chan struct{}
	informer  informercorev1.ConfigMapInformer
	synced    bool
}

type featureConfig struct {
	Enabled     bool   `yaml:"enabled"`
	Description string `yaml:"description"`
}

// NewFeatureListFromConfigMap creates a new feature flag list from the 'feautre-flags' configmap in the provided namespace
// Feature configuration must be stored in the features key in the configmap
func NewFeatureListFromConfigMap(ctx context.Context, clientset kubernetes.Interface, namespace string) (*FeatureList, error) {
	lg, err := zap.NewDevelopment()
	if err != nil {
		panic(err)
	}

	cm, err := clientset.CoreV1().ConfigMaps(namespace).Get(ctx, "feature-flags", metav1.GetOptions{})
	if err != nil {
		return &FeatureList{}, err
	}

	features, err := buildFeatureList(cm)
	if err != nil {
		return &FeatureList{}, err
	}

	factory := informers.NewSharedInformerFactoryWithOptions(
		clientset,
		time.Minute,
		informers.WithNamespace(namespace),
	)
	informer := factory.Core().V1().ConfigMaps()

	return &FeatureList{
		ctx:       ctx,
		logger:    lg.Named("feature-flags"),
		features:  features,
		clientset: clientset,
		queue:     workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()),
		notify:    make(chan struct{}),
		informer:  informer,
	}, nil
}

// GetFeature returns a named feature flag
func (l *FeatureList) GetFeature(name string) FeatureFlag {
	for _, feature := range l.features {
		if feature.Name() == name {
			return feature
		}
	}
	return nil
}

// Update and notify will create a watch on the configmap and return a notfiy channel
// that gets written to when the configmap is updated
func (l *FeatureList) WatchConfigMap() context.CancelFunc {
	defer runtime.HandleCrash()

	ctx, cancel := context.WithCancel(l.ctx)

	mux := &sync.RWMutex{}

	l.informer.Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				mux.RLock()
				defer mux.RUnlock()
				if !l.synced {
					return
				}
				cm := obj.(*corev1.ConfigMap)
				if cm.Name != "feature-flags" {
					return
				}

				l.enqueueEvent(obj)
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				mux.RLock()
				defer mux.RUnlock()
				if !l.synced {
					return
				}
				cm := newObj.(*corev1.ConfigMap)
				if cm.Name != "feature-flags" {
					return
				}
				new := newObj.(*corev1.ConfigMap)
				old := oldObj.(*corev1.ConfigMap)
				if new.ResourceVersion == old.ResourceVersion {
					return
				}

				l.enqueueEvent(newObj)
			},
			DeleteFunc: nil,
		},
	)

	l.logger.Info("starting configmap watch collector")
	go l.informer.Informer().Run(ctx.Done())

	cacheSynced := cache.WaitForNamedCacheSync("feature-flags", l.ctx.Done(), l.informer.Informer().HasSynced)
	mux.Lock()
	l.synced = cacheSynced
	mux.Unlock()

	if !cacheSynced {
		defer cancel()
		close(l.notify)
		l.logger.Error("failed to wait for caches")
		return cancel
	}

	go wait.UntilWithContext(ctx, func(ctx context.Context) {
		for l.processNexItem(ctx.Done()) {
		}
	}, time.Second)

	return cancel
}

func (l *FeatureList) NotifyChange() <-chan struct{} {
	return l.notify
}

func (l *FeatureList) enqueueEvent(obj interface{}) {
	_, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		l.logger.Error(fmt.Sprintf("could't get key for event %+v: %s", obj, err))
	}
	l.queue.Add(obj)
}

func (l *FeatureList) processNexItem(stopCh <-chan struct{}) bool {
	select {
	case <-stopCh:
		close(l.notify)
		l.queue.ShutDown()
		return false
	default:
	}

	event, shutdown := l.queue.Get()
	if shutdown {
		return false
	}
	defer l.queue.Done(event)

	err := l.processItem(event)
	if err != nil {
		l.logger.Error(fmt.Sprintf("failed to process event %s, giving up: %v", event, err))
		l.queue.Forget(event)
		utilruntime.HandleError(err)
		return true
	}

	l.queue.Forget(event)
	return true
}

func (l *FeatureList) processItem(obj interface{}) error {
	cm := obj.(*corev1.ConfigMap)

	features, err := buildFeatureList(cm)
	if err != nil {
		return err
	}
	l.logger.Debug("sending notification")
	l.features = features
	l.notify <- struct{}{}
	return nil
}

func buildFeatureList(cm *corev1.ConfigMap) ([]FeatureFlag, error) {
	var features []FeatureFlag
	featuresConfig := map[string]featureConfig{}

	config, ok := cm.Data["features"]
	if !ok {
		return features, errors.New("missing features key in configmap")
	}

	err := yaml.Unmarshal([]byte(config), &featuresConfig)
	if err != nil {
		return features, err
	}

	for name, config := range featuresConfig {
		feature := NewFeature(name, config.Description, config.Enabled)
		features = append(features, &feature)
	}

	return features, nil
}
