// The MIT License
//
// Copyright (c) 2024 Temporal Technologies Inc.  All rights reserved.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package callbacks

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
	"time"

	"github.com/nexus-rpc/sdk-go/nexus"
	commonpb "go.temporal.io/api/common/v1"

	"go.temporal.io/server/common/metrics"
	"go.temporal.io/server/common/namespace"
	"go.temporal.io/server/service/history/hsm"
	"go.temporal.io/server/service/history/queues"
)

var retryable4xxErrorTypes = []int{
	http.StatusRequestTimeout,
	http.StatusTooManyRequests,
}

type CanGetNexusCompletion interface {
	GetNexusCompletion(ctx context.Context) (nexus.OperationCompletion, error)
}

// HTTPCaller is a method that can be used to invoke HTTP requests.
type HTTPCaller func(*http.Request) (*http.Response, error)

func RegisterExecutor(
	registry *hsm.Registry,
	activeExecutorOptions ActiveExecutorOptions,
	standbyExecutorOptions StandbyExecutorOptions,
	config *Config,
) error {
	activeExec := activeExecutor{options: activeExecutorOptions, config: config}
	standbyExec := standbyExecutor{options: standbyExecutorOptions}
	if err := hsm.RegisterExecutors(
		registry,
		TaskTypeInvocation.ID,
		activeExec.executeInvocationTask,
		standbyExec.executeInvocationTask,
	); err != nil {
		return err
	}
	return hsm.RegisterExecutors(
		registry,
		TaskTypeBackoff.ID,
		activeExec.executeBackoffTask,
		standbyExec.executeBackoffTask,
	)
}

type (
	ActiveExecutorOptions struct {
		NamespaceRegistry namespace.Registry
		MetricsHandler    metrics.Handler
		CallerProvider    func(queues.NamespaceIDAndDestination) HTTPCaller
	}

	activeExecutor struct {
		options ActiveExecutorOptions
		config  *Config
	}
)

func (e activeExecutor) executeInvocationTask(
	ctx context.Context,
	env hsm.Environment,
	ref hsm.Ref,
	task InvocationTask,
) error {
	ns, err := e.options.NamespaceRegistry.GetNamespaceByID(namespace.ID(ref.WorkflowKey.NamespaceID))
	if err != nil {
		return fmt.Errorf("failed to get namespace by ID: %w", err)
	}

	args, err := e.loadInvocationArgs(ctx, env, ref)
	if err != nil {
		return err
	}

	callCtx, cancel := context.WithTimeout(
		ctx,
		e.config.RequestTimeout(ns.Name().String(), task.Destination),
	)
	defer cancel()

	request, err := nexus.NewCompletionHTTPRequest(callCtx, args.url, args.completion)
	if request.Header == nil {
		request.Header = make(http.Header)
	}
	for k, v := range args.header {
		request.Header.Set(k, v)
	}
	if err != nil {
		return queues.NewUnprocessableTaskError(
			fmt.Sprintf("failed to construct Nexus request: %v", err),
		)
	}

	caller := e.options.CallerProvider(queues.NamespaceIDAndDestination{
		NamespaceID: ref.WorkflowKey.GetNamespaceID(),
		Destination: task.Destination,
	})
	// Make the call and record metrics.
	startTime := time.Now()
	response, callErr := caller(request)

	namespaceTag := metrics.NamespaceTag(ns.Name().String())
	destTag := metrics.DestinationTag(task.Destination)
	statusCodeTag := metrics.NexusOutcomeTag(outcomeTag(callCtx, response, callErr))
	e.options.MetricsHandler.Counter(RequestCounter.Name()).Record(1, namespaceTag, destTag, statusCodeTag)
	e.options.MetricsHandler.Timer(RequestLatencyHistogram.Name()).Record(time.Since(startTime), namespaceTag, destTag, statusCodeTag)

	if callErr == nil {
		// Body is not read but should be discarded to keep the underlying TCP connection alive.
		// Just in case something unexpected happens while discarding or closing the body,
		// propagate errors to the machine.
		if _, callErr = io.Copy(io.Discard, response.Body); callErr == nil {
			callErr = response.Body.Close()
		}
	}

	err = e.saveResult(ctx, env, ref, response, callErr)

	if callErr != nil {
		err = queues.NewDestinationDownError(callErr.Error(), err)
	} else if isRetryableHTTPResponse(response) {
		err = queues.NewDestinationDownError(
			fmt.Sprintf("response returned retryable status code %d", response.StatusCode),
			err,
		)
	}
	return err
}

type invocationArgs struct {
	url        string
	header     map[string]string
	completion nexus.OperationCompletion
}

func (e activeExecutor) loadInvocationArgs(
	ctx context.Context,
	env hsm.Environment,
	ref hsm.Ref,
) (args invocationArgs, err error) {
	err = env.Access(ctx, ref, hsm.AccessRead, func(node *hsm.Node) error {
		callback, err := hsm.MachineData[Callback](node)
		if err != nil {
			return err
		}
		target, err := hsm.MachineData[CanGetNexusCompletion](node.Parent)
		if err != nil {
			return err
		}
		switch variant := callback.PublicInfo.GetCallback().GetVariant().(type) {
		case *commonpb.Callback_Nexus_:
			args.url = variant.Nexus.GetUrl()
			args.header = variant.Nexus.GetHeader()
			args.completion, err = target.GetNexusCompletion(ctx)
			if err != nil {
				return err
			}
		default:
			return queues.NewUnprocessableTaskError(
				fmt.Sprintf("unprocessable callback variant: %v", variant),
			)
		}
		return nil
	})
	return
}

func (e activeExecutor) saveResult(
	ctx context.Context,
	env hsm.Environment,
	ref hsm.Ref,
	response *http.Response,
	callErr error,
) error {
	return env.Access(ctx, ref, hsm.AccessWrite, func(node *hsm.Node) error {
		return hsm.MachineTransition(node, func(callback Callback) (hsm.TransitionOutput, error) {
			if callErr == nil {
				if response.StatusCode >= 200 && response.StatusCode < 300 {
					return TransitionSucceeded.Apply(callback, EventSucceeded{})
				}
				callErr = fmt.Errorf("request failed with: %v", response.Status) // nolint:goerr113
				if !isRetryableHTTPResponse(response) {
					return TransitionFailed.Apply(callback, EventFailed{
						Time: env.Now(),
						Err:  callErr,
					})
				}
			}
			return TransitionAttemptFailed.Apply(callback, EventAttemptFailed{
				Time:        env.Now(),
				Err:         callErr,
				RetryPolicy: e.config.RetryPolicy(),
			})
		})
	})
}

func (e activeExecutor) executeBackoffTask(
	ctx context.Context,
	env hsm.Environment,
	ref hsm.Ref,
	task BackoffTask,
) error {
	return env.Access(ctx, ref, hsm.AccessWrite, func(node *hsm.Node) error {
		return hsm.MachineTransition(node, func(callback Callback) (hsm.TransitionOutput, error) {
			return TransitionRescheduled.Apply(callback, EventRescheduled{})
		})
	})
}

type (
	StandbyExecutorOptions struct{}

	standbyExecutor struct {
		options StandbyExecutorOptions
	}
)

func (e standbyExecutor) executeInvocationTask(
	ctx context.Context,
	env hsm.Environment,
	ref hsm.Ref,
	task InvocationTask,
) error {
	panic("unimplemented")
}

func (e standbyExecutor) executeBackoffTask(
	ctx context.Context,
	env hsm.Environment,
	ref hsm.Ref,
	task BackoffTask,
) error {
	panic("unimplemented")
}

func isRetryableHTTPResponse(response *http.Response) bool {
	return response.StatusCode >= 500 || slices.Contains(retryable4xxErrorTypes, response.StatusCode)
}

func outcomeTag(callCtx context.Context, response *http.Response, callErr error) string {
	if callErr != nil {
		if callCtx.Err() != nil {
			return "request-timeout"
		}
		return "unknown-error"
	}
	return fmt.Sprintf("status:%d", response.StatusCode)
}
