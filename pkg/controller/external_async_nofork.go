// SPDX-FileCopyrightText: 2023 The Crossplane Authors <https://crossplane.io>
//
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"time"

	"github.com/crossplane/crossplane-runtime/pkg/logging"
	"github.com/crossplane/crossplane-runtime/pkg/meta"
	"github.com/crossplane/crossplane-runtime/pkg/reconciler/managed"
	xpresource "github.com/crossplane/crossplane-runtime/pkg/resource"
	"github.com/pkg/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/crossplane/upjet/pkg/config"
	"github.com/crossplane/upjet/pkg/controller/handler"
	"github.com/crossplane/upjet/pkg/metrics"
	"github.com/crossplane/upjet/pkg/resource"
	"github.com/crossplane/upjet/pkg/terraform"
	tferrors "github.com/crossplane/upjet/pkg/terraform/errors"
)

var defaultAsyncTimeout = 1 * time.Hour

type NoForkAsyncConnector struct {
	*NoForkConnector
	callback     CallbackProvider
	eventHandler *handler.EventHandler
}

type NoForkAsyncOption func(connector *NoForkAsyncConnector)

func NewNoForkAsyncConnector(kube client.Client, ots *OperationTrackerStore, sf terraform.SetupFn, cfg *config.Resource, opts ...NoForkAsyncOption) *NoForkAsyncConnector {
	nfac := &NoForkAsyncConnector{
		NoForkConnector: NewNoForkConnector(kube, sf, cfg, ots),
	}
	for _, f := range opts {
		f(nfac)
	}
	return nfac
}

func (c *NoForkAsyncConnector) Connect(ctx context.Context, mg xpresource.Managed) (managed.ExternalClient, error) {
	ec, err := c.NoForkConnector.Connect(ctx, mg)
	if err != nil {
		return nil, errors.Wrap(err, "cannot initialize the no-fork async external client")
	}

	return &noForkAsyncExternal{
		noForkExternal: ec.(*noForkExternal),
		callback:       c.callback,
		eventHandler:   c.eventHandler,
	}, nil
}

// WithNoForkAsyncConnectorEventHandler configures the EventHandler so that
// the no-fork external clients can requeue reconciliation requests.
func WithNoForkAsyncConnectorEventHandler(e *handler.EventHandler) NoForkAsyncOption {
	return func(c *NoForkAsyncConnector) {
		c.eventHandler = e
	}
}

// WithNoForkAsyncCallbackProvider configures the controller to use async variant of the functions
// of the Terraform client and run given callbacks once those operations are
// completed.
func WithNoForkAsyncCallbackProvider(ac CallbackProvider) NoForkAsyncOption {
	return func(c *NoForkAsyncConnector) {
		c.callback = ac
	}
}

// WithNoForkAsyncLogger configures a logger for the NoForkAsyncConnector.
func WithNoForkAsyncLogger(l logging.Logger) NoForkAsyncOption {
	return func(c *NoForkAsyncConnector) {
		c.logger = l
	}
}

// WithNoForkAsyncMetricRecorder configures a metrics.MetricRecorder for the
// NoForkAsyncConnector.
func WithNoForkAsyncMetricRecorder(r *metrics.MetricRecorder) NoForkAsyncOption {
	return func(c *NoForkAsyncConnector) {
		c.metricRecorder = r
	}
}

// WithNoForkAsyncManagementPolicies configures whether the client should
// handle management policies.
func WithNoForkAsyncManagementPolicies(isManagementPoliciesEnabled bool) NoForkAsyncOption {
	return func(c *NoForkAsyncConnector) {
		c.isManagementPoliciesEnabled = isManagementPoliciesEnabled
	}
}

type noForkAsyncExternal struct {
	*noForkExternal
	callback     CallbackProvider
	eventHandler *handler.EventHandler
}

type CallbackFn func(error, context.Context) error

func (n *noForkAsyncExternal) Observe(ctx context.Context, mg xpresource.Managed) (managed.ExternalObservation, error) {
	if n.opTracker.LastOperation.IsRunning() {
		n.logger.WithValues("opType", n.opTracker.LastOperation.Type).Debug("ongoing async operation")
		return managed.ExternalObservation{
			ResourceExists:   true,
			ResourceUpToDate: true,
		}, nil
	}
	n.opTracker.LastOperation.Flush()

	o, err := n.noForkExternal.Observe(ctx, mg)
	// clear any previously reported LastAsyncOperation error condition here,
	// because there are no pending updates on the existing resource and it's
	// not scheduled to be deleted.
	if err == nil && o.ResourceExists && o.ResourceUpToDate && !meta.WasDeleted(mg) {
		mg.(resource.Terraformed).SetConditions(resource.LastAsyncOperationCondition(nil))
	}
	return o, err
}

func (n *noForkAsyncExternal) Create(_ context.Context, mg xpresource.Managed) (managed.ExternalCreation, error) {
	if !n.opTracker.LastOperation.MarkStart("create") {
		return managed.ExternalCreation{}, errors.Errorf("%s operation that started at %s is still running", n.opTracker.LastOperation.Type, n.opTracker.LastOperation.StartTime().String())
	}

	ctx, cancel := context.WithDeadline(context.Background(), n.opTracker.LastOperation.StartTime().Add(defaultAsyncTimeout))
	go func() {
		defer cancel()

		n.opTracker.logger.Debug("Async create starting...", "tfID", n.opTracker.GetTfID())
		_, err := n.noForkExternal.Create(ctx, mg)
		err = tferrors.NewAsyncCreateFailed(err)
		n.opTracker.LastOperation.SetError(err)
		n.opTracker.logger.Debug("Async create ended.", "error", err, "tfID", n.opTracker.GetTfID())

		n.opTracker.LastOperation.MarkEnd()
		if cErr := n.callback.Create(mg.GetName())(err, ctx); cErr != nil {
			n.opTracker.logger.Info("Async create callback failed", "error", cErr.Error())
		}
	}()

	return managed.ExternalCreation{}, nil
}

func (n *noForkAsyncExternal) Update(_ context.Context, mg xpresource.Managed) (managed.ExternalUpdate, error) {
	if !n.opTracker.LastOperation.MarkStart("update") {
		return managed.ExternalUpdate{}, errors.Errorf("%s operation that started at %s is still running", n.opTracker.LastOperation.Type, n.opTracker.LastOperation.StartTime().String())
	}

	ctx, cancel := context.WithDeadline(context.Background(), n.opTracker.LastOperation.StartTime().Add(defaultAsyncTimeout))
	go func() {
		defer cancel()

		n.opTracker.logger.Debug("Async update starting...", "tfID", n.opTracker.GetTfID())
		_, err := n.noForkExternal.Update(ctx, mg)
		err = tferrors.NewAsyncUpdateFailed(err)
		n.opTracker.LastOperation.SetError(err)
		n.opTracker.logger.Debug("Async update ended.", "error", err, "tfID", n.opTracker.GetTfID())

		n.opTracker.LastOperation.MarkEnd()
		if cErr := n.callback.Update(mg.GetName())(err, ctx); cErr != nil {
			n.opTracker.logger.Info("Async update callback failed", "error", cErr.Error())
		}
	}()

	return managed.ExternalUpdate{}, nil
}

func (n *noForkAsyncExternal) Delete(_ context.Context, mg xpresource.Managed) error {
	switch {
	case n.opTracker.LastOperation.Type == "delete":
		n.opTracker.logger.Debug("The previous delete operation is still ongoing", "tfID", n.opTracker.GetTfID())
		return nil
	case !n.opTracker.LastOperation.MarkStart("delete"):
		return errors.Errorf("%s operation that started at %s is still running", n.opTracker.LastOperation.Type, n.opTracker.LastOperation.StartTime().String())
	}

	ctx, cancel := context.WithDeadline(context.Background(), n.opTracker.LastOperation.StartTime().Add(defaultAsyncTimeout))
	go func() {
		defer cancel()

		n.opTracker.logger.Debug("Async delete starting...", "tfID", n.opTracker.GetTfID())
		err := tferrors.NewAsyncDeleteFailed(n.noForkExternal.Delete(ctx, mg))
		n.opTracker.LastOperation.SetError(err)
		n.opTracker.logger.Debug("Async delete ended.", "error", err, "tfID", n.opTracker.GetTfID())

		n.opTracker.LastOperation.MarkEnd()
		if cErr := n.callback.Destroy(mg.GetName())(err, ctx); cErr != nil {
			n.opTracker.logger.Info("Async delete callback failed", "error", cErr.Error())
		}
	}()

	return nil
}
