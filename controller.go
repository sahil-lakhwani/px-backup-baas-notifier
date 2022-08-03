package main

import (
	"context"
	"sync"
	"time"

	"github.com/portworx/px-backup-baas-notifier/pkg/notification"
	"github.com/portworx/px-backup-baas-notifier/pkg/schedule"
	"github.com/portworx/px-backup-baas-notifier/pkg/types"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

type controller struct {
	client              kubernetes.Interface
	dynclient           dynamic.Interface
	nsinformer          cache.SharedIndexInformer
	backupinformer      cache.SharedIndexInformer
	mongoinformer       cache.SharedIndexInformer
	dynInformer         dynamicinformer.DynamicSharedInformerFactory
	backupLister        cache.GenericLister
	mongoLister         cache.GenericLister
	stopChannel         <-chan struct{}
	fullCacheSyncedOnce bool
	stateHistory        *StateHistory
	notifyClient        notification.Client
	nsqueue             workqueue.RateLimitingInterface
	backupqueue         workqueue.RateLimitingInterface
	deletionqueue       workqueue.RateLimitingInterface
	schedule            schedule.Schedule
}

var backupGVR = schema.GroupVersionResource{
	Group:    "backup.purestorage.com",
	Version:  "v1alpha1",
	Resource: "backups",
}

var mongoGVR = schema.GroupVersionResource{
	Group:    "backup.purestorage.com",
	Version:  "v1alpha1",
	Resource: "mongos",
}

// type NotificationRetryStatus struct {
// 	needsRetry bool
// 	backoff    time.Duration // default 2 min and doubles for every retry
// 	id         string
// }

type NamespaceStateHistory struct {
	backupStatus    types.Status
	mongoStatus     types.Status
	schedulerStatus types.Status
	notification    string
	lastUpdate      time.Time
}

type StateHistory struct {
	sync.RWMutex
	perNamespaceHistory map[string]*NamespaceStateHistory
}

// Create Informers and add eventhandlers
func newController(client kubernetes.Interface, dynclient dynamic.Interface,
	dynInformer dynamicinformer.DynamicSharedInformerFactory,
	stopch <-chan struct{}, notifyClient notification.Client, schedule schedule.Schedule) *controller {

	nsqueue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())
	deletionqueue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())
	backupqueue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())
	// notificationretryqueue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())
	Backupinf := dynInformer.ForResource(backupGVR).Informer()
	Mongoinf := dynInformer.ForResource(mongoGVR).Informer()

	ctx := context.Background()
	// labelstring := strings.Split(nsLabel, ":")
	// labelSelector := labels.Set(map[string]string{labelstring[0]: labelstring[1]}).AsSelector()

	nsinformer := cache.NewSharedIndexInformer(
		&cache.ListWatch{
			ListFunc: func(options v1.ListOptions) (runtime.Object, error) {
				// options.LabelSelector = labelSelector.String()
				return client.CoreV1().Namespaces().List(ctx, v1.ListOptions{})
			},
			WatchFunc: func(options v1.ListOptions) (watch.Interface, error) {
				// options.LabelSelector = labelSelector.String()
				return client.CoreV1().Namespaces().Watch(ctx, v1.ListOptions{})
			},
		},
		&corev1.Namespace{},
		0, //Skip resync
		cache.Indexers{},
	)

	c := &controller{
		dynclient:      dynclient,
		client:         client,
		nsinformer:     nsinformer,
		backupinformer: Backupinf,
		mongoinformer:  Mongoinf,
		dynInformer:    dynInformer,
		backupLister:   dynInformer.ForResource(backupGVR).Lister(),
		mongoLister:    dynInformer.ForResource(mongoGVR).Lister(),
		stopChannel:    stopch,
		stateHistory: &StateHistory{
			perNamespaceHistory: map[string]*NamespaceStateHistory{},
		},
		notifyClient:  notifyClient,
		nsqueue:       nsqueue,
		backupqueue:   backupqueue,
		schedule:      schedule,
		deletionqueue: deletionqueue,
		// notificationretryqueue: notificationretryqueue,
	}

	nsinformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		DeleteFunc: func(obj interface{}) {
			Logger.Info("Delete namespace event", "NameSpace", obj.(*corev1.Namespace).Name)
			c.nsqueue.Add(obj)
		},
	})

	Backupinf.AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				Logger.Info("Backup CREATE event")
				c.SyncInformerCache()
				Logger.Info("Backup CREATE event")
				c.backupqueue.Add(obj)
			},
			UpdateFunc: func(old, new interface{}) {
				Logger.Info("Backup UPDATE event")
				c.backupqueue.Add(new)
			},
			DeleteFunc: func(obj interface{}) {
				Logger.Info("Backup is deleted")
				c.deletionqueue.Add(obj)
				// c.handleCRDeletion(obj)
			},
		},
	)

	Mongoinf.AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				Logger.Info("Mongo CREATE event")
				c.SyncInformerCache()
				Logger.Info("Mongo CREATE event")
				c.backupqueue.Add(obj)
			},
			UpdateFunc: func(old, new interface{}) {
				Logger.Info("Mongo UPDATE event")
				c.backupqueue.Add(new)
			},
			DeleteFunc: func(obj interface{}) {
				Logger.Info("Mongo is deleted")
				c.deletionqueue.Add(obj)
				// c.handleCRDeletion(obj)
			},
		},
	)
	return c
}

func (c *controller) SyncInformerCache() {
	if !c.fullCacheSyncedOnce {
		if !cache.WaitForNamedCacheSync("px-backup-notifier", c.stopChannel, c.backupinformer.HasSynced,
			c.nsinformer.HasSynced, c.mongoinformer.HasSynced) {
			Logger.Info("Timedout waiting for cache to be synced")
			c.fullCacheSyncedOnce = false
			return
		}
	}
	c.fullCacheSyncedOnce = true
}

// start the controller
func (c *controller) run(stopch <-chan struct{}) {
	Logger.Info("Started notification controller")
	defer c.nsqueue.ShutDown()
	defer c.backupqueue.ShutDown()
	defer c.deletionqueue.ShutDown()

	c.dynInformer.Start(stopch)
	go c.nsinformer.Run(stopch)

	go wait.Until(c.nsworker, 1*time.Second, stopch)

	go wait.Until(c.crworker, 1*time.Second, stopch)

	go wait.Until(c.deletionWorker, 1*time.Second, stopch)

	// go wait.Until(c.notificationRetryWorker, 1*time.Second, stopch)

	<-stopch

	Logger.Info("Shutting down notification controller")

}

func (c *controller) nsworker() {
	for c.handleNamespaceDeletion() {
	}
}

func (c *controller) crworker() {
	for c.handleBackupAndMongoCreateUpdateEvents() {
	}
}

func (c *controller) deletionWorker() {
	for c.handleCRDeletion() {
	}
}

func (c *controller) handleNamespaceDeletion() bool {
	item, quit := c.nsqueue.Get()
	if quit {
		return false
	}
	defer c.nsqueue.Done(item)
	key, err := cache.MetaNamespaceKeyFunc(item)
	if err != nil {
		Logger.Error(err, "getting key from cahce")
		return false
	}

	_, namespace, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		Logger.Error(err, "splitting key into namespace and name")
		return false
	}

	if _, exists, _ := c.nsinformer.GetIndexer().GetByKey(key); exists {
		Logger.Info("requeuing namespace deletion...", "Namespace", namespace)
		c.nsqueue.AddAfter(item, 10*time.Second)
		return true
	}

	Logger.Info("Namespace deletion completed", "Namespace", namespace)

	var previousState types.Status
	sh, ok := c.stateHistory.perNamespaceHistory[namespace]
	if !ok {
		previousState = types.Status("Unknown")
	} else {
		previousState = types.Status(sh.notification)
	}

	if !c.sendNotification(namespace, previousState, types.DELETED) {
		c.nsqueue.AddAfter(item, 10*time.Second)
		return true
	}

	c.updateStateHistory(namespace, NamespaceStateHistory{
		notification: string(types.DELETED),
		backupStatus: types.DELETED,
		mongoStatus:  types.DELETED,
	})
	c.nsqueue.Forget(key)

	return true
}

func (c *controller) handleCRDeletion() bool {

	item, quit := c.deletionqueue.Get()
	if quit {
		return false
	}
	defer c.deletionqueue.Done(item)

	u := item.(*unstructured.Unstructured)
	ns := u.Object["metadata"].(map[string]interface{})["namespace"].(string)

	var mongoStatus, backupStatus types.Status
	backupStatus = getCRStatus(c.backupLister, ns)
	mongoStatus = getCRStatus(c.mongoLister, ns)

	if (backupStatus == types.NOTFOUND && mongoStatus == types.AVAILABLE) ||
		(backupStatus == types.AVAILABLE && mongoStatus == types.NOTFOUND) &&
			(c.stateHistory.perNamespaceHistory[ns].notification == "Success") {

		var previousState types.Status
		sh, ok := c.stateHistory.perNamespaceHistory[ns]
		if !ok {
			previousState = types.Status("Unknown")
		} else {
			previousState = types.Status(sh.notification)
		}

		if !c.sendNotification(ns, previousState, types.NotReachable) {
			c.deletionqueue.AddAfter(item, 10*time.Second)
			return true
		}

		c.updateStateHistory(ns, NamespaceStateHistory{
			notification: string(types.NotReachable),
			backupStatus: backupStatus,
			mongoStatus:  mongoStatus,
		})

	}
	return true
}

func (c *controller) handleBackupAndMongoCreateUpdateEvents() bool {

	var mongoStatus, backupStatus, schedulerStatus types.Status
	item, quit := c.backupqueue.Get()
	if quit {

		return false
	}
	defer c.backupqueue.Done(item)

	u := item.(*unstructured.Unstructured)
	ns := u.GetNamespace()
	creationTime := u.GetCreationTimestamp()

	backupStatus = getCRStatus(c.backupLister, ns)
	mongoStatus = getCRStatus(c.mongoLister, ns)

	state := notification.StatesAndNotificationsMapping[string(backupStatus)][string(mongoStatus)]

	if ((backupStatus == types.NOTFOUND && mongoStatus != types.NOTFOUND) ||
		(backupStatus != types.NOTFOUND && mongoStatus == types.NOTFOUND)) &&
		(time.Since(creationTime.Time) < time.Duration(1*time.Minute)) {
		// It might happen that Mongo Cr is created and backup is not created yet or vice versa,
		// In this case we dont want to send unreachable or deleted straightway
		// following e.g. scenarios
		// Mongo --> Pending/Available/NotReachable   AND   Backup --> types.NOTFOUND
		// then we send Provisioning till creationTime < 2 min
		// same goes for below case
		// Backup --> Pending/Available/ and Mongo --> types.NOTFOUND
		// Idea here is to wait for 2 minutes before we send notification as defined in StatesAndNotificationsMapping \
		// because CR creation might be delayed or cache has not been sync properly
		state = "Provisioning"
	}

	if state == "Success" {
		// schedulerStatus, err := c.schedule.GetStatus(creationTime, ns)
		// if err != nil {
		// 	Logger.Error(err, "Failed to get scheduler status", "namespace", ns)
		// }
		schedulerStatus = types.AVAILABLE
		Logger.Info("", "SchedulerStatus", schedulerStatus, "NameSpace", ns)
		state = notification.BackupAndSchedulerStatusMapping[string(types.AVAILABLE)][string(schedulerStatus)]
		if state == "Provisioning" {
			defer c.backupqueue.AddAfter(item, time.Duration(retryDelaySeconds)*time.Second)
		} else {
			defer c.backupqueue.Forget(item)
		}
	}

	var previousState types.Status
	sh, ok := c.stateHistory.perNamespaceHistory[ns]
	if !ok {
		previousState = types.Status("Unknown")
	} else {
		previousState = types.Status(sh.notification)
	}

	c.updateStateHistory(ns, NamespaceStateHistory{
		backupStatus:    backupStatus,
		mongoStatus:     mongoStatus,
		schedulerStatus: schedulerStatus,
	})

	if !c.sendNotification(ns, previousState, types.Status(state)) {
		c.backupqueue.AddAfter(item, time.Duration(retryDelaySeconds)*time.Second)
		return true
	}

	c.stateHistory.perNamespaceHistory[ns].notification = state

	return true
}

func (c *controller) updateStateHistory(ns string, new NamespaceStateHistory) {

	Logger.Info("updating", "namespace", ns, "new", new)

	c.stateHistory.Lock()

	// if new.schedulerStatus == "" && c.stateHistory.perNamespaceHistory[ns].schedulerStatus != "" {
	// 	// we only query schedulerStatus
	// 	// so if we dont know schedulerStatus in current reconcilation check for previous
	// 	new.schedulerStatus = c.stateHistory.perNamespaceHistory[ns].schedulerStatus
	// }

	new.lastUpdate = time.Now()
	c.stateHistory.perNamespaceHistory[ns] = &new

	c.stateHistory.Unlock()
}

func (c *controller) sendNotification(ns string, previousState, newState types.Status) bool {

	Logger.Info("Sending notification", "NameSpace", ns, "PreviousState", previousState, "NewState", newState)

	if previousState == newState {
		return true
	}

	note := notification.Note{
		State:     string(newState),
		Namespace: ns,
	}

	if err := c.notifyClient.Send(note); err != nil {
		Logger.Error(err, "Failed to send notification", "namespace", ns)
		return false
	}
	return true
}

func getCRStatus(lister cache.GenericLister, ns string) types.Status {
	cr, _ := lister.ByNamespace(ns).List(labels.NewSelector())
	if len(cr) != 0 {
		u := cr[0].(*unstructured.Unstructured)
		status := extractStateFromCRStatus(u.Object)
		return types.Status(status)
	}
	return types.NOTFOUND
}

func extractStateFromCRStatus(obj map[string]interface{}) string {
	var state string

	if obj["status"] == nil {
		state = "Pending"
	} else {
		state = obj["status"].(map[string]interface{})["state"].(string)
	}
	return state
}
