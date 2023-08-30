/*
Copyright 2019 The Kubernetes Authors.

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

package flowcontrol

import (
	"context"
	"strconv"
	"strings"
	"time"

	endpointsrequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/server/httplog"
	"k8s.io/apiserver/pkg/server/mux"
	fq "k8s.io/apiserver/pkg/util/flowcontrol/fairqueuing"
	"k8s.io/apiserver/pkg/util/flowcontrol/fairqueuing/eventclock"
	fqs "k8s.io/apiserver/pkg/util/flowcontrol/fairqueuing/queueset"
	"k8s.io/apiserver/pkg/util/flowcontrol/metrics"
	fcrequest "k8s.io/apiserver/pkg/util/flowcontrol/request"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"

	flowcontrol "k8s.io/api/flowcontrol/v1beta3"
	flowcontrolclient "k8s.io/client-go/kubernetes/typed/flowcontrol/v1beta3"
)

// ConfigConsumerAsFieldManager is how the config consuminng
// controller appears in an ObjectMeta ManagedFieldsEntry.Manager
const ConfigConsumerAsFieldManager = "api-priority-and-fairness-config-consumer-v1"

// Interface defines how the API Priority and Fairness filter interacts with the underlying system.
type Interface interface {
	// Handle takes care of queuing and dispatching a request
	// characterized by the given digest.  The given `noteFn` will be
	// invoked with the results of request classification.
	// The given `workEstimator` is called, if at all, after noteFn.
	// `workEstimator` will be invoked only when the request
	//  is classified as non 'exempt'.
	// 'workEstimator', when invoked, must return the
	// work parameters for the request.
	// If the request is queued then `queueNoteFn` will be called twice,
	// first with `true` and then with `false`; otherwise
	// `queueNoteFn` will not be called at all.  If Handle decides
	// that the request should be executed then `execute()` will be
	// invoked once to execute the request; otherwise `execute()` will
	// not be invoked.
	// Handle() should never return while execute() is running, even if
	// ctx is cancelled or times out.
	Handle(ctx context.Context,
		requestDigest RequestDigest,
		noteFn func(fs *flowcontrol.FlowSchema, pl *flowcontrol.PriorityLevelConfiguration, flowDistinguisher string),
		workEstimator func() fcrequest.WorkEstimate,
		queueNoteFn fq.QueueNoteFn,
		execFn func(),
	)

	// Run monitors config objects from the main apiservers and causes
	// any needed changes to local behavior.  This method ceases
	// activity and returns after the given channel is closed.
	Run(stopCh <-chan struct{}) error

	// Install installs debugging endpoints to the web-server.
	Install(c *mux.PathRecorderMux)

	// WatchTracker provides the WatchTracker interface.
	WatchTracker
}

// This request filter implements https://github.com/kubernetes/enhancements/blob/master/keps/sig-api-machinery/1040-priority-and-fairness/README.md

// New creates a new instance to implement API priority and fairness
func New(
	informerFactory kubeinformers.SharedInformerFactory,
	flowcontrolClient flowcontrolclient.FlowcontrolV1beta3Interface,
	serverConcurrencyLimit int,
	requestWaitLimit time.Duration,
) Interface {
	clk := eventclock.Real{}
	return NewTestable(TestableConfig{
		Name:                   "Controller",
		Clock:                  clk,
		AsFieldManager:         ConfigConsumerAsFieldManager,
		FoundToDangling:        func(found bool) bool { return !found },
		InformerFactory:        informerFactory,
		FlowcontrolClient:      flowcontrolClient,
		ServerConcurrencyLimit: serverConcurrencyLimit,
		RequestWaitLimit:       requestWaitLimit,
		ReqsGaugeVec:           metrics.PriorityLevelConcurrencyGaugeVec,
		ExecSeatsGaugeVec:      metrics.PriorityLevelExecutionSeatsGaugeVec,
		QueueSetFactory:        fqs.NewQueueSetFactory(clk),
	})
}

// TestableConfig carries the parameters to an implementation that is testable
type TestableConfig struct {
	// Name of the controller
	Name string

	// Clock to use in timing deliberate delays
	Clock clock.PassiveClock

	// AsFieldManager is the string to use in the metadata for
	// server-side apply.  Normally this is
	// `ConfigConsumerAsFieldManager`.  This is exposed as a parameter
	// so that a test of competing controllers can supply different
	// values.
	AsFieldManager string

	// FoundToDangling maps the boolean indicating whether a
	// FlowSchema's referenced PLC exists to the boolean indicating
	// that FlowSchema's status should indicate a dangling reference.
	// This is a parameter so that we can write tests of what happens
	// when servers disagree on that bit of Status.
	FoundToDangling func(bool) bool

	// InformerFactory to use in building the controller
	InformerFactory kubeinformers.SharedInformerFactory

	// FlowcontrolClient to use for manipulating config objects
	FlowcontrolClient flowcontrolclient.FlowcontrolV1beta3Interface

	// ServerConcurrencyLimit for the controller to enforce
	ServerConcurrencyLimit int

	// RequestWaitLimit configured on the server
	RequestWaitLimit time.Duration

	// GaugeVec for metrics about requests, broken down by phase and priority_level
	ReqsGaugeVec metrics.RatioedGaugeVec

	// RatioedGaugePairVec for metrics about seats occupied by all phases of execution
	ExecSeatsGaugeVec metrics.RatioedGaugeVec

	// QueueSetFactory for the queuing implementation
	QueueSetFactory fq.QueueSetFactory
}

// NewTestable is extra flexible to facilitate testing
func NewTestable(config TestableConfig) Interface {
	return newTestableController(config)
}

func (cfgCtlr *configController) Handle(ctx context.Context, requestDigest RequestDigest,
	noteFn func(fs *flowcontrol.FlowSchema, pl *flowcontrol.PriorityLevelConfiguration, flowDistinguisher string),
	workEstimator func() fcrequest.WorkEstimate,
	queueNoteFn fq.QueueNoteFn,
	execFn func()) {
	// Print when a request just entered the APIserver
	// Only for the empty application that we're investigating
	// We give an example for each request - and be as specific in the selection statements
	if requestDigest.RequestInfo.Verb == "create" &&
		requestDigest.RequestInfo.Namespace == "default" &&
		requestDigest.RequestInfo.Resource == "jobs" &&
		requestDigest.RequestInfo.Subresource == "" &&
		requestDigest.RequestInfo.Name == "" &&
		requestDigest.User.GetName() == "kubernetes-admin" {
		// Kubectl sent a request to create a new job
		//
		// RequestDigest{
		// 		RequestInfo: &request.RequestInfo{
		//			IsResourceRequest:true,
		//			Path:"/apis/batch/v1/namespaces/default/jobs",
		// 			Verb:"create",
		//			APIPrefix:"apis",
		//			APIGroup:"batch",
		//			APIVersion:"v1",
		//			Namespace:"default",
		//			Resource:"jobs",
		//			Subresource:"",
		//			Name:"",
		//			Parts:[]string{"jobs"}},
		//		User: &user.DefaultInfo{
		//			Name:"kubernetes-admin",
		//			UID:"",
		// 			Groups:[]string{"system:masters", "system:authenticated"},
		//			Extra:map[string][]string(nil)}}
		klog.Infof("%s [CONTINUUM] 0200", time.Now().UnixNano())
	} else if requestDigest.RequestInfo.Verb == "get" &&
		requestDigest.RequestInfo.Namespace == "kube-system" &&
		requestDigest.RequestInfo.Resource == "serviceaccounts" &&
		requestDigest.RequestInfo.Subresource == "" &&
		requestDigest.RequestInfo.Name == "job-controller" &&
		requestDigest.User.GetName() == "system:kube-controller-manager" {
		// The job-controller reads the just requested job
		//
		// RequestDigest{
		// 		RequestInfo: &request.RequestInfo{
		// 			IsResourceRequest:true,
		// 			Path:"/api/v1/namespaces/kube-system/serviceaccounts/job-controller",
		// 			Verb:"get",
		// 			APIPrefix:"api",
		// 			APIGroup:"",
		// 			APIVersion:"v1",
		// 			Namespace:"kube-system",
		// 			Resource:"serviceaccounts",
		// 			Subresource:"",
		// 			Name:"job-controller",
		// 			Parts:[]string{"serviceaccounts", "job-controller"}},
		// 		User: &user.DefaultInfo{
		// 			Name:"system:kube-controller-manager",
		// 			UID:"",
		// 			Groups:[]string{"system:authenticated"},
		// 			Extra:map[string][]string(nil)}}
		klog.Infof("%s [CONTINUUM] 0202", time.Now().UnixNano())
	} else if requestDigest.RequestInfo.Verb == "create" &&
		requestDigest.RequestInfo.Namespace == "default" &&
		requestDigest.RequestInfo.Resource == "pods" &&
		requestDigest.RequestInfo.Subresource == "" &&
		requestDigest.RequestInfo.Name == "" &&
		requestDigest.User.GetName() == "system:serviceaccount:kube-system:job-controller" {
		// Creating the pod for the job-controller
		//
		// RequestDigest{
		//  	RequestInfo: &request.RequestInfo{
		// 			IsResourceRequest:true,
		// 			Path:"/api/v1/namespaces/default/pods",
		// 			Verb:"create",
		// 			APIPrefix:"api",
		// 			APIGroup:"",
		// 			APIVersion:"v1",
		// 			Namespace:"default",
		// 			Resource:"pods",
		// 			Subresource:"",
		// 			Name:"",
		// 			Parts:[]string{"pods"}},
		// 		User: &user.DefaultInfo{
		// 			Name:"system:serviceaccount:kube-system:job-controller",
		// 			UID:"7f26f97f-9541-48d0-860e-a8517db5489d",
		// 			Groups:[]string{"system:serviceaccounts", "system:serviceaccounts:kube-system", "system:authenticated"},
		// 			Extra:map[string][]string(nil)}}
		klog.Infof("%s [CONTINUUM] 0204", time.Now().UnixNano())
	} else if requestDigest.RequestInfo.Verb == "create" &&
		requestDigest.RequestInfo.Namespace == "default" &&
		requestDigest.RequestInfo.Resource == "pods" &&
		requestDigest.RequestInfo.Subresource == "binding" &&
		strings.Contains(requestDigest.RequestInfo.Name, "empty") &&
		requestDigest.User.GetName() == "system:kube-scheduler" {
		// Scheduler creates the binding from pod to node
		//
		// RequestDigest{
		// 		RequestInfo: &request.RequestInfo{
		// 			IsResourceRequest:true,
		// 			Path:"/api/v1/namespaces/default/pods/empty-gp574/binding",
		// 			Verb:"create",
		// 			APIPrefix:"api",
		// 			APIGroup:"",
		// 			APIVersion:"v1",
		// 			Namespace:"default",
		// 			Resource:"pods",
		// 			Subresource:"binding",
		// 			Name:"empty-gp574",
		// 			Parts:[]string{"pods", "empty-gp574", "binding"}},
		// 		User: &user.DefaultInfo{
		// 			Name:"system:kube-scheduler",
		// 			UID:"",
		// 			Groups:[]string{"system:authenticated"},
		// 			Extra:map[string][]string(nil)}}
		klog.Infof("%s [CONTINUUM] 0206", time.Now().UnixNano())
	} else if requestDigest.RequestInfo.Verb == "get" &&
		requestDigest.RequestInfo.Namespace == "default" &&
		requestDigest.RequestInfo.Resource == "pods" &&
		requestDigest.RequestInfo.Subresource == "" &&
		strings.Contains(requestDigest.RequestInfo.Name, "empty") &&
		strings.Contains(requestDigest.User.GetName(), "system:node:") {
		// Kubelet on worker node reads the pod
		//
		// RequestDigest{
		// 		RequestInfo: &request.RequestInfo{
		// 			IsResourceRequest:true,
		// 			Path:"/api/v1/namespaces/default/pods/empty-gp574",
		// 			Verb:"get",
		// 			APIPrefix:"api",
		// 			APIGroup:"",
		// 			APIVersion:"v1",
		// 			Namespace:"default",
		// 			Resource:"pods",
		// 			Subresource:"",
		// 			Name:"empty-gp574",
		// 			Parts:[]string{"pods", "empty-gp574"}},
		// 		User: &user.DefaultInfo{
		// 			Name:"system:node:cloud0matthijs",
		// 			UID:"",
		// 			Groups:[]string{"system:nodes", "system:authenticated"},
		// 			Extra:map[string][]string(nil)}}
		klog.Infof("%s [CONTINUUM] 0208", time.Now().UnixNano())
	}

	fs, pl, isExempt, req, startWaitingTime := cfgCtlr.startRequest(ctx, requestDigest, noteFn, workEstimator, queueNoteFn)
	queued := startWaitingTime != time.Time{}
	if req == nil {
		if queued {
			observeQueueWaitTime(ctx, pl.Name, fs.Name, strconv.FormatBool(req != nil), cfgCtlr.clock.Since(startWaitingTime))
		}
		klog.V(7).Infof("Handle(%#+v) => fsName=%q, distMethod=%#+v, plName=%q, isExempt=%v, reject", requestDigest, fs.Name, fs.Spec.DistinguisherMethod, pl.Name, isExempt)
		return
	}
	klog.V(7).Infof("Handle(%#+v) => fsName=%q, distMethod=%#+v, plName=%q, isExempt=%v, queued=%v", requestDigest, fs.Name, fs.Spec.DistinguisherMethod, pl.Name, isExempt, queued)
	var executed bool
	idle, panicking := true, true
	defer func() {
		// Print when a request has succesfully been processed by the APIserver
		// Only for the empty application that we're investigating
		// Similar to the prints at the start of this function, just other numbers to indicate finish
		if requestDigest.RequestInfo.Verb == "create" &&
			requestDigest.RequestInfo.Namespace == "default" &&
			requestDigest.RequestInfo.Resource == "jobs" &&
			requestDigest.RequestInfo.Subresource == "" &&
			requestDigest.RequestInfo.Name == "" &&
			requestDigest.User.GetName() == "kubernetes-admin" {
			// Kubectl sent a request to create a new job
			klog.Infof("%s [CONTINUUM] 0201", time.Now().UnixNano())
		} else if requestDigest.RequestInfo.Verb == "get" &&
			requestDigest.RequestInfo.Namespace == "kube-system" &&
			requestDigest.RequestInfo.Resource == "serviceaccounts" &&
			requestDigest.RequestInfo.Subresource == "" &&
			requestDigest.RequestInfo.Name == "job-controller" &&
			requestDigest.User.GetName() == "system:kube-controller-manager" {
			// The job-controller reads the just requested job
			klog.Infof("%s [CONTINUUM] 0203", time.Now().UnixNano())
		} else if requestDigest.RequestInfo.Verb == "create" &&
			requestDigest.RequestInfo.Namespace == "default" &&
			requestDigest.RequestInfo.Resource == "pods" &&
			requestDigest.RequestInfo.Subresource == "" &&
			requestDigest.RequestInfo.Name == "" &&
			requestDigest.User.GetName() == "system:serviceaccount:kube-system:job-controller" {
			klog.Infof("%s [CONTINUUM] 0205", time.Now().UnixNano())
		} else if requestDigest.RequestInfo.Verb == "create" &&
			requestDigest.RequestInfo.Namespace == "default" &&
			requestDigest.RequestInfo.Resource == "pods" &&
			requestDigest.RequestInfo.Subresource == "binding" &&
			strings.Contains(requestDigest.RequestInfo.Name, "empty") &&
			requestDigest.User.GetName() == "system:kube-scheduler" {
			klog.Infof("%s [CONTINUUM] 0207", time.Now().UnixNano())
		} else if requestDigest.RequestInfo.Verb == "get" &&
			requestDigest.RequestInfo.Namespace == "default" &&
			requestDigest.RequestInfo.Resource == "pods" &&
			requestDigest.RequestInfo.Subresource == "" &&
			strings.Contains(requestDigest.RequestInfo.Name, "empty") &&
			strings.Contains(requestDigest.User.GetName(), "system:node:") {
			klog.Infof("%s [CONTINUUM] 0209", time.Now().UnixNano())
		}

		klog.V(7).Infof("Handle(%#+v) => fsName=%q, distMethod=%#+v, plName=%q, isExempt=%v, queued=%v, Finish() => panicking=%v idle=%v",
			requestDigest, fs.Name, fs.Spec.DistinguisherMethod, pl.Name, isExempt, queued, panicking, idle)
		if idle {
			cfgCtlr.maybeReap(pl.Name)
		}
	}()
	idle = req.Finish(func() {
		if queued {
			observeQueueWaitTime(ctx, pl.Name, fs.Name, strconv.FormatBool(req != nil), cfgCtlr.clock.Since(startWaitingTime))
		}
		metrics.AddDispatch(ctx, pl.Name, fs.Name)
		fqs.OnRequestDispatched(req)
		executed = true
		startExecutionTime := cfgCtlr.clock.Now()
		defer func() {
			executionTime := cfgCtlr.clock.Since(startExecutionTime)
			httplog.AddKeyValue(ctx, "apf_execution_time", executionTime)
			metrics.ObserveExecutionDuration(ctx, pl.Name, fs.Name, executionTime)
		}()
		execFn()
	})
	if queued && !executed {
		observeQueueWaitTime(ctx, pl.Name, fs.Name, strconv.FormatBool(req != nil), cfgCtlr.clock.Since(startWaitingTime))
	}
	panicking = false
}

func observeQueueWaitTime(ctx context.Context, priorityLevelName, flowSchemaName, execute string, waitTime time.Duration) {
	metrics.ObserveWaitingDuration(ctx, priorityLevelName, flowSchemaName, execute, waitTime)
	endpointsrequest.TrackAPFQueueWaitLatency(ctx, waitTime)
}
