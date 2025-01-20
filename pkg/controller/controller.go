package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"k8s.io/klog/v2"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

// Controller demonstrates how to implement a controller with client-go.
type Controller struct {
	indexer  cache.Indexer
	queue    workqueue.TypedRateLimitingInterface[string]
	informer cache.Controller

	dynClient *dynamic.DynamicClient
	disClient *discovery.DiscoveryClient
	clientset *kubernetes.Clientset

	mapper meta.RESTMapper
}

// NewController creates a new Controller.
func NewController(queue workqueue.TypedRateLimitingInterface[string], indexer cache.Indexer, informer cache.Controller, dynClient *dynamic.DynamicClient, disClient *discovery.DiscoveryClient, clientset *kubernetes.Clientset) *Controller {
	return &Controller{
		informer:  informer,
		indexer:   indexer,
		queue:     queue,
		dynClient: dynClient,
		disClient: disClient,
		clientset: clientset,
	}
}

func (c *Controller) processNextItem() bool {
	// Wait until there is a new item in the working queue
	key, quit := c.queue.Get()
	if quit {
		return false
	}
	// Tell the queue that we are done with processing this key. This unblocks the key for other workers
	// This allows safe parallel processing because two pods with the same key are never processed in
	// parallel.
	defer c.queue.Done(key)

	// Invoke the method containing the business logic
	err := c.discoverResourceRelationships(key)
	// Handle the error if something went wrong during the execution of the business logic
	c.handleErr(err, key)
	return true
}

// discoverResourceRelationships is the business logic of the controller. In this controller it tries to find resource relationships
// The retry logic should not be part of the business logic.
func (c *Controller) discoverResourceRelationships(key string) error {
	obj, exists, err := c.indexer.GetByKey(key)
	if err != nil {
		klog.Errorf("fetching object with key %s from store failed with %v", key, err)
		return err
	}

	if !exists {
		// Below we will warm up our cache with a event, so that we will see a delete for one event
		klog.Infof("event %s does not exist anymore\n", key)
	} else {
		// Note that you also have to check the uid if you have a local controlled resource, which
		// is dependent on the actual instance, to detect that a Pod was recreated with the same name
		gvk := (obj.(*v1.Event)).InvolvedObject.GroupVersionKind()
		objName := (obj.(*v1.Event)).InvolvedObject.Name
		objNamespace := (obj.(*v1.Event)).InvolvedObject.Namespace
		klog.Infof("received event %s:%s\n", gvk.Group, gvk.Kind)
		ownerRefs, err := c.getOwnerReferences(gvk.GroupKind(), objName, objNamespace)
		if err != nil {
			klog.Errorf("fetching owner references for object with key %s failed with %v", key, err)
			return err
		}
		for _, ownerRef := range ownerRefs {
			klog.Infof("Owner ref: %s", ownerRef.GroupKind())
			c.addResourceRelationData(ownerRef.GroupKind().String(), gvk.GroupKind().String())
		}
	}
	return nil
}

// handleErr checks if an error happened and makes sure we will retry later.
func (c *Controller) handleErr(err error, key string) {
	if err == nil {
		// Forget about the #AddRateLimited history of the key on every successful synchronization.
		// This ensures that future processing of updates for this key is not delayed because of
		// an outdated error history.
		c.queue.Forget(key)
		return
	}

	// This controller retries 5 times if something goes wrong. After that, it stops trying.
	if c.queue.NumRequeues(key) < 5 {
		klog.Infof("Error syncing event %v: %v", key, err)

		// Re-enqueue the key rate limited. Based on the rate limiter on the
		// queue and the re-enqueue history, the key will be processed later again.
		c.queue.AddRateLimited(key)
		return
	}

	c.queue.Forget(key)
	// Report to an external entity that, even after several retries, we could not successfully process this key
	runtime.HandleError(err)
	klog.Infof("Dropping event %q out of the queue: %v", key, err)
}

// Run begins watching and syncing.
func (c *Controller) Run(workers int, stopCh chan struct{}) {
	defer runtime.HandleCrash()

	// Let the workers stop when we are done
	defer c.queue.ShutDown()
	klog.Info("Starting Event controller")

	go c.informer.Run(stopCh)

	// Wait for all involved caches to be synced, before processing items from the queue is started
	if !cache.WaitForCacheSync(stopCh, c.informer.HasSynced) {
		runtime.HandleError(fmt.Errorf("timed out waiting for caches to sync"))
		return
	}

	for i := 0; i < workers; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	<-stopCh
	klog.Info("Stopping Event controller")
}

func (c *Controller) runWorker() {
	for c.processNextItem() {
	}
}

// getOwnerReferences returns the owner references of a given group kind string
// and object name
func (c *Controller) getOwnerReferences(gk schema.GroupKind, name, namespace string) ([]*schema.GroupVersionKind, error) {
	if c.mapper == nil {
		err := c.initRESTMapper()
		if err != nil {
			return nil, err
		}
	}
	mapping, err := c.mapper.RESTMapping(gk)
	if err != nil {
		// There could be new CRDs created, re-initialize the mapper and check again
		err := c.initRESTMapper()
		if err != nil {
			return nil, err
		}
		mapping, err = c.mapper.RESTMapping(gk)
		if err != nil {
			return nil, err
		}
	}
	obj, err := c.dynClient.Resource(mapping.Resource).Namespace(namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	owners := obj.GetOwnerReferences()
	results := make([]*schema.GroupVersionKind, len(owners))
	for i, owner := range owners {
		apiVersionParts := strings.Split(owner.APIVersion, "/")
		group := apiVersionParts[0]
		version := apiVersionParts[1]
		results[i] = &schema.GroupVersionKind{Group: group, Version: version, Kind: owner.Kind}
	}
	return results, nil
}

func (c *Controller) addResourceRelationData(key, value string) error {
	cm, err := c.clientset.CoreV1().ConfigMaps("argocd").Get(context.Background(), "resource-relation-lookup", metav1.GetOptions{})
	if err != nil {
		return err
	}
	if cm.Data == nil {
		cm.Data = make(map[string]string)
	}
	if _, ok := cm.Data[key]; !ok {
		cm.Data[key] = value
		_, err := c.clientset.CoreV1().ConfigMaps("argocd").Update(context.Background(), cm, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *Controller) initRESTMapper() error {
	groupResources, err := restmapper.GetAPIGroupResources(c.disClient)
	if err != nil {
		return err
	}
	for _, groupResource := range groupResources {
		klog.Infof("group/kind resource parsed %s", groupResource.Group.Name)
	}
	c.mapper = restmapper.NewDiscoveryRESTMapper(groupResources)
	return nil
}
