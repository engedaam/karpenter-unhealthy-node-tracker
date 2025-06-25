package main

import (
	"context"
	"encoding/csv"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	controllerruntime "sigs.k8s.io/controller-runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/operator/logging"
)

var unhealthyCondition = corev1.NodeCondition{
	Type:   corev1.NodeConditionType("TestTypeReady"),
	Status: corev1.ConditionFalse,
}
var tolerationDuration = 30 * time.Second

func main() {
	ctrllog.SetLogger(logging.NopLogger)
	ctx := context.Background()
	config := ctrl.GetConfigOrDie()
	config.QPS = 5000
	config.Burst = 5000
	mgr := lo.Must(controllerruntime.NewManager(config, controllerruntime.Options{}))

	writer := csv.NewWriter(os.Stdout)
	writer.WriteAll([][]string{{"Event Type", "Node", "Duration (ms)"}})

	unhealthyNodeTime := &sync.Map{}
	nodeWatcher := &nodeWatcher{kubeClient: mgr.GetClient(), writer: writer, unhealthyNodeTime: unhealthyNodeTime, startDeleteNodeTime: &sync.Map{}, endDeleteNodeTime: &sync.Map{}}
	nodeClaimWatcher := &nodeClaimWatcher{kubeClient: mgr.GetClient(), writer: writer, unhealthyNodeTime: unhealthyNodeTime, nodeClaimToNodeName: &sync.Map{}, startDeleteNodeClaimTime: &sync.Map{}, endDeleteNodeClaimTime: &sync.Map{}, instanceTerminatingTime: &sync.Map{}, nodeDrainCompletedTime: &sync.Map{}}
	lo.Must0(nodeWatcher.SetupWithManager(mgr))
	lo.Must0(nodeClaimWatcher.SetupWithManager(mgr))
	lo.Must0(mgr.Start(ctx))
}

type nodeWatcher struct {
	kubeClient          client.Client
	writer              *csv.Writer
	unhealthyNodeTime   *sync.Map
	startDeleteNodeTime *sync.Map
	endDeleteNodeTime   *sync.Map
	once                sync.Once
}

func (*nodeWatcher) Name() string {
	return "node.watcher"
}

func (c *nodeWatcher) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	c.once.Do(func() {
		log.Printf("Started Node Watcher...")
	})
	node := &corev1.Node{}
	if err := c.kubeClient.Get(ctx, request.NamespacedName, node); err != nil {
		if errors.IsNotFound(err) {
			// If we haven't previously seen the node getting deleted, then we use the current time as the startDeleteTime
			startDeleteTime, loaded := c.startDeleteNodeTime.LoadOrStore(request.NamespacedName.String(), time.Now())
			unhealthyTime, ok := c.unhealthyNodeTime.Load(request.NamespacedName.String())
			// If we haven't previously seen the node getting deleted, then we need to log the StartNodeDelete event
			if ok {
				if !loaded {
					c.writer.WriteAll([][]string{{"StartNodeDelete", node.Name, fmt.Sprint((startDeleteTime.(time.Time).Sub(unhealthyTime.(time.Time)) - tolerationDuration).Milliseconds())}})
				}
				c.writer.WriteAll([][]string{{"EndNodeDelete", node.Name, fmt.Sprint((time.Since(unhealthyTime.(time.Time)) - tolerationDuration).Milliseconds())}})
			}
			c.endDeleteNodeTime.Store(request.NamespacedName, time.Now())
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}
	if cond := GetCondition(node, unhealthyCondition.Type); cond.Status == corev1.ConditionFalse {
		c.unhealthyNodeTime.LoadOrStore(client.ObjectKeyFromObject(node).String(), cond.LastTransitionTime.Time)
	}
	if !node.DeletionTimestamp.IsZero() {
		unhealthyTime, ok := c.unhealthyNodeTime.Load(client.ObjectKeyFromObject(node).String())
		_, ok = unhealthyTime.(time.Time)
		if ok {
			_, loaded := c.startDeleteNodeTime.LoadOrStore(client.ObjectKeyFromObject(node).String(), node.DeletionTimestamp.Time)
			if !loaded {
				c.writer.WriteAll([][]string{{"StartNodeDelete", node.Name, fmt.Sprint((node.DeletionTimestamp.Time.Sub(unhealthyTime.(time.Time)) - tolerationDuration).Milliseconds())}})
			}
		}
	}
	return reconcile.Result{}, nil
}

func (c *nodeWatcher) SetupWithManager(mgr manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(mgr).
		Named(c.Name()).
		For(&corev1.Node{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1000, CacheSyncTimeout: time.Minute * 5}).
		Complete(c)
}

type nodeClaimWatcher struct {
	kubeClient               client.Client
	writer                   *csv.Writer
	nodeClaimToNodeName      *sync.Map
	unhealthyNodeTime        *sync.Map
	startDeleteNodeClaimTime *sync.Map
	endDeleteNodeClaimTime   *sync.Map
	instanceTerminatingTime  *sync.Map
	nodeDrainCompletedTime   *sync.Map
	once                     sync.Once
}

func (*nodeClaimWatcher) Name() string {
	return "nodeclaim.watcher"
}

func (c *nodeClaimWatcher) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	c.once.Do(func() {
		log.Printf("Started NodeClaim Watcher...")
	})
	nodeClaim := &v1.NodeClaim{}
	if err := c.kubeClient.Get(ctx, request.NamespacedName, nodeClaim); err != nil {
		if errors.IsNotFound(err) {
			nodeName, ok := c.nodeClaimToNodeName.Load(request.NamespacedName)
			if !ok {
				return reconcile.Result{}, nil
			}
			// If we haven't previously seen the nodeclaim getting deleted, then we use the current time as the startDeleteTime
			startDeleteTime, loaded := c.startDeleteNodeClaimTime.LoadOrStore(request.NamespacedName.String(), time.Now())
			unhealthyTime, ok := c.unhealthyNodeTime.Load(types.NamespacedName{Name: nodeName.(string)}.String())
			// If we haven't previously seen the node getting deleted, then we need to log the StartNodeClaimDelete event
			if ok {
				if !loaded {
					c.writer.WriteAll([][]string{{"StartNodeClaimDelete", nodeName.(string), fmt.Sprint((startDeleteTime.(time.Time).Sub(unhealthyTime.(time.Time)) - tolerationDuration).Milliseconds())}})
				}
				c.writer.WriteAll([][]string{{"EndNodeClaimDelete", nodeName.(string), fmt.Sprint((time.Since(unhealthyTime.(time.Time)) - tolerationDuration).Milliseconds())}})
			}
			c.endDeleteNodeClaimTime.Store(request.NamespacedName, time.Now())
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}
	c.nodeClaimToNodeName.Store(request.NamespacedName, nodeClaim.Status.NodeName)
	if !nodeClaim.DeletionTimestamp.IsZero() {
		unhealthyTime, ok := c.unhealthyNodeTime.Load(types.NamespacedName{Name: nodeClaim.Status.NodeName}.String())
		if ok {
			_, loaded := c.startDeleteNodeClaimTime.LoadOrStore(client.ObjectKeyFromObject(nodeClaim).String(), nodeClaim.DeletionTimestamp.Time)
			if !loaded {
				c.writer.WriteAll([][]string{{"StartNodeClaimDelete", nodeClaim.Status.NodeName, fmt.Sprint((nodeClaim.DeletionTimestamp.Time.Sub(unhealthyTime.(time.Time)) - tolerationDuration).Milliseconds())}})
			}
		}
	}
	if cond := nodeClaim.StatusConditions().Get(v1.ConditionTypeInstanceTerminating); cond.IsTrue() {
		unhealthyTime, ok := c.unhealthyNodeTime.Load(types.NamespacedName{Name: nodeClaim.Status.NodeName}.String())
		if ok {
			_, loaded := c.instanceTerminatingTime.LoadOrStore(request.NamespacedName, cond.LastTransitionTime.Time)
			if !loaded {
				c.writer.WriteAll([][]string{{"StartInstanceTerminating", nodeClaim.Status.NodeName, fmt.Sprint((cond.LastTransitionTime.Sub(unhealthyTime.(time.Time)) - tolerationDuration).Milliseconds())}})
			}
		}
	}
	if cond := nodeClaim.StatusConditions().Get(v1.ConditionTypeDrained); cond.IsTrue() {
		unhealthyTime, ok := c.unhealthyNodeTime.Load(types.NamespacedName{Name: nodeClaim.Status.NodeName}.String())
		if ok {
			_, loaded := c.nodeDrainCompletedTime.LoadOrStore(request.NamespacedName, cond.LastTransitionTime.Time)
			if !loaded {
				c.writer.WriteAll([][]string{{"EndNodeDrain", nodeClaim.Status.NodeName, fmt.Sprint((cond.LastTransitionTime.Sub(unhealthyTime.(time.Time)) - tolerationDuration).Milliseconds())}})
			}
		}
	}
	return reconcile.Result{}, nil
}

func (c *nodeClaimWatcher) SetupWithManager(mgr manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(mgr).
		Named(c.Name()).
		For(&v1.NodeClaim{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1000, CacheSyncTimeout: time.Minute * 5}).
		Complete(c)
}

func GetCondition(n *corev1.Node, match corev1.NodeConditionType) corev1.NodeCondition {
	cond, _ := lo.Find(n.Status.Conditions, func(c corev1.NodeCondition) bool {
		return c.Type == match
	})
	return cond
}
