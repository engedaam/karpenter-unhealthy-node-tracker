package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/awslabs/operatorpkg/singleton"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/util/workqueue"
	controllerruntime "sigs.k8s.io/controller-runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/karpenter/pkg/operator/logging"
	utilsnode "sigs.k8s.io/karpenter/pkg/utils/node"
)

var healthyCondition = corev1.NodeCondition{
	Type:   corev1.NodeConditionType("TestTypeReady"),
	Status: corev1.ConditionFalse,
}
var tolerationDuration = time.Minute

func main() {
	log.SetLogger(logging.NopLogger)
	ctx := context.Background()
	config := ctrl.GetConfigOrDie()
	config.QPS = 5000
	config.Burst = 5000
	mgr := lo.Must(controllerruntime.NewManager(config, controllerruntime.Options{Logger: log.FromContext(ctx)}))
	c := mgr.GetClient()
	lo.Must0(mgr.GetFieldIndexer().IndexField(ctx, &corev1.Node{}, "status.phase", func(o client.Object) []string {
		return []string{string(o.(*corev1.Node).Status.Phase)}
	}), "failed to setup pod indexer")
	watcher := &nodeWatcher{kubeClient: c, unhealthyNodes: map[string]time.Time{}}
	fmt.Println("Node Watcher has started")
	lo.Must0(watcher.Builder(mgr))
	lo.Must0(mgr.Start(ctx))
}

type nodeWatcher struct {
	kubeClient     client.Client
	unhealthyNodes map[string]time.Time
	mu             sync.RWMutex
}

func (*nodeWatcher) Name() string {
	return "node.watcher"
}

func (c *nodeWatcher) Reconcile(ctx context.Context) (reconcile.Result, error) {
	nodeList := &corev1.NodeList{}
	lo.Must0(c.kubeClient.List(ctx, nodeList))

	workqueue.ParallelizeUntil(ctx, 1000, len(nodeList.Items), func(i int) {
		node := nodeList.Items[i]
		c.mu.RLock()
		unhealthyTime, found := c.unhealthyNodes[node.Name]
		c.mu.RUnlock()
		if !node.DeletionTimestamp.IsZero() && found {
			fmt.Printf("%s, %.2f\n", node.Name, (node.DeletionTimestamp.Time.Sub(unhealthyTime) - tolerationDuration).Seconds())
			c.mu.Lock()
			delete(c.unhealthyNodes, node.Name)
			c.mu.Unlock()
		}

		nodeHealthyCondition := utilsnode.GetCondition(&node, healthyCondition.Type)
		if nodeHealthyCondition.Type != healthyCondition.Type || nodeHealthyCondition.Status != healthyCondition.Status {
			return
		} else if nodeHealthyCondition.Status == healthyCondition.Status && !found {
			c.mu.Lock()
			c.unhealthyNodes[node.Name] = nodeHealthyCondition.LastTransitionTime.Time
			c.mu.Unlock()
		}
	})
	removedNodes := []string{}
	workqueue.ParallelizeUntil(ctx, 500, len(lo.Keys(c.unhealthyNodes)), func(i int) {
		nodeName := lo.Keys(c.unhealthyNodes)[i]
		if !lo.ContainsBy(nodeList.Items, func(n corev1.Node) bool { return n.Name == nodeName }) {
			fmt.Printf("%s, %.2f\n", nodeName, (time.Now().Sub(c.unhealthyNodes[nodeName]) - tolerationDuration).Seconds())
			c.mu.Lock()
			removedNodes = append(removedNodes, nodeName)
			c.mu.Unlock()
		}
	})

	for _, rn := range removedNodes {
		c.mu.Lock()
		delete(c.unhealthyNodes, rn)
		c.mu.Unlock()
	}

	return reconcile.Result{RequeueAfter: 5 * time.Second}, nil
}

func (c *nodeWatcher) Builder(mgr manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(mgr).
		Named(c.Name()).
		WatchesRawSource(singleton.Source()).
		Complete(singleton.AsReconciler(c))
}
