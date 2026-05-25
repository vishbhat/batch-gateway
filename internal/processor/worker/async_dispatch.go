/*
Copyright 2026 The llm-d Authors

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

package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/google/uuid"
	asyncapi "github.com/llm-d-incubation/llm-d-async/api"
	asyncprod "github.com/llm-d-incubation/llm-d-async/producer"

	"github.com/llm-d-incubation/batch-gateway/internal/processor/config"
	"github.com/llm-d-incubation/batch-gateway/internal/processor/metrics"
	batch_types "github.com/llm-d-incubation/batch-gateway/internal/shared/types"
	"github.com/llm-d-incubation/batch-gateway/internal/util/logging"
)

const (
	defaultPerRequestTimeout = 10 * time.Minute
	// collectorBackstopTimeout is the maximum time to wait for remaining results
	// after all submissions are done. Prevents hanging if results are lost/delayed.
	collectorBackstopTimeout = 2 * time.Minute
)

// pendingRequest tracks an in-flight async request.
type pendingRequest struct {
	customID string
	poolName string
	modelID  string
	timer    *time.Timer
}

// asyncDispatcher submits inference requests to llm-d-async and collects their
// results, writing output lines using the same writers/progress as the sync path.
// One asyncDispatcher is created per job; it is shared across all model goroutines.
type asyncDispatcher struct {
	producers map[string]asyncprod.Producer // pool name → producer

	mu         sync.Mutex
	inflight   map[string]*pendingRequest // batch_req_<uuid> → pending
	poolCounts map[string]int             // pool name → in-flight count

	writers  *outputWriters
	progress *executionProgress

	collectDone     chan struct{} // closed when all pool collectors finish
	collectWg       sync.WaitGroup
	submissionsDone chan struct{} // closed after all submissions complete (wg.Wait done)

	pollTimeout       time.Duration
	perRequestTimeout time.Duration

	cfg           *config.ProcessorConfig
	jobID         string
	tenantID      string
	sloCtx        context.Context
	userCancelCtx context.Context

	logger logr.Logger
}

// newAsyncDispatcher creates an asyncDispatcher for a single job.
func newAsyncDispatcher(
	producers map[string]asyncprod.Producer,
	writers *outputWriters,
	progress *executionProgress,
	cfg *config.ProcessorConfig,
	jobID, tenantID string,
	sloCtx, userCancelCtx context.Context,
	logger logr.Logger,
) *asyncDispatcher {
	return &asyncDispatcher{
		producers:         producers,
		inflight:          make(map[string]*pendingRequest),
		poolCounts:        make(map[string]int),
		writers:           writers,
		progress:          progress,
		collectDone:       make(chan struct{}),
		submissionsDone:   make(chan struct{}),
		pollTimeout:       cfg.AsyncDispatchConfig.ResultPollTimeout,
		perRequestTimeout: defaultPerRequestTimeout,
		cfg:               cfg,
		jobID:             jobID,
		tenantID:          tenantID,
		sloCtx:            sloCtx,
		userCancelCtx:     userCancelCtx,
		logger:            logger,
	}
}

// startCollectors launches one collectResults goroutine per pool and closes
// collectDone when all finish.
func (d *asyncDispatcher) startCollectors(ctx context.Context, poolNames []string) {
	for _, poolName := range poolNames {
		d.collectWg.Add(1)
		go d.collectResults(ctx, poolName)
	}
	go func() {
		d.collectWg.Wait()
		close(d.collectDone)
	}()
}

// signalSubmissionsDone closes the submissionsDone channel so collectors can
// exit once all in-flight entries for their pool are resolved. Must be called
// exactly once, after all submission goroutines have finished.
func (d *asyncDispatcher) signalSubmissionsDone() {
	close(d.submissionsDone)
}

// submitRequest reads one plan entry, builds a RequestMessage, submits it to
// the pool's producer, and registers it in the in-flight tracker.
// Errors (parse, submit failure, missing pool) are written as error output lines.
// No semaphore acquisition — the dispatcher controls flow downstream.
func (d *asyncDispatcher) submitRequest(
	ctx context.Context,
	sloCtx context.Context,
	inputFile *os.File,
	entry planEntry,
	modelID string,
	passThroughHeaders map[string]string,
) {
	buf := make([]byte, entry.Length)
	if _, err := inputFile.ReadAt(buf, entry.Offset); err != nil {
		d.logger.Error(fmt.Errorf("%w at offset %d: %w", errRequestInputRead, entry.Offset, err),
			"Failed to read request input")
		batchReqID := newBatchRequestID(uuid.NewString())
		d.writeErrorLine(ctx, &outputLine{
			ID:    batchReqID,
			Error: &outputError{Code: string(batch_types.ErrCodeBatchFailed), Message: "failed to read request from input file"},
		})
		d.progress.record(ctx, false)
		return
	}

	trimmed := bytes.TrimSuffix(buf, []byte{'\n'})
	requestID := uuid.NewString()
	batchReqID := newBatchRequestID(requestID)

	var req batch_types.Request
	if err := json.Unmarshal(trimmed, &req); err != nil {
		d.logger.Error(err, "Failed to parse request line")
		d.writeErrorLine(ctx, &outputLine{
			ID:    batchReqID,
			Error: &outputError{Code: string(batch_types.ErrCodeBatchFailed), Message: fmt.Sprintf("failed to parse request: %v", err)},
		})
		d.progress.record(ctx, false)
		return
	}

	poolName := d.cfg.InferencePoolNameFor(modelID)
	if poolName == "" {
		d.logger.V(logging.INFO).Info("No pool configured for model, recording as model_not_found", "model", modelID)
		d.writeErrorLine(ctx, &outputLine{
			ID:       batchReqID,
			CustomID: req.CustomID,
			Error:    &outputError{Code: "model_not_found", Message: fmt.Sprintf("model %q is not configured for async dispatch", modelID)},
		})
		d.progress.record(ctx, false)
		metrics.RecordRequestError(modelID)
		return
	}

	prod, ok := d.producers[poolName]
	if !ok {
		d.logger.V(logging.INFO).Info("No producer for pool", "pool", poolName)
		d.writeErrorLine(ctx, &outputLine{
			ID:       batchReqID,
			CustomID: req.CustomID,
			Error:    &outputError{Code: string(batch_types.ErrCodeBatchFailed), Message: fmt.Sprintf("no producer for pool %q", poolName)},
		})
		d.progress.record(ctx, false)
		return
	}

	fairnessID := ""
	if d.cfg.SendFairnessHeader {
		fairnessID = d.tenantID
	}
	headers := maps.Clone(passThroughHeaders)
	headers = mergeInferenceHeaders(headers, sloCtx, d.cfg.InferenceObjectiveFor(modelID), fairnessID)

	var deadline int64
	if dl, ok := sloCtx.Deadline(); ok {
		deadline = dl.Unix()
	} else {
		deadline = time.Now().Add(defaultPerRequestTimeout).Unix()
	}

	msg := &asyncapi.RequestMessage{
		ID:       batchReqID,
		Created:  time.Now().Unix(),
		Deadline: deadline,
		Payload:  req.Body,
		Headers:  headers,
		Endpoint: req.URL,
		Metadata: map[string]string{
			"job_id":       d.jobID,
			"input_offset": strconv.FormatInt(entry.Offset, 10),
		},
	}

	if err := prod.SubmitRequest(ctx, msg); err != nil {
		d.logger.Error(err, "Failed to submit async request", "customId", req.CustomID)
		d.writeErrorLine(ctx, &outputLine{
			ID:       batchReqID,
			CustomID: req.CustomID,
			Error:    &outputError{Code: string(batch_types.ErrCodeBatchFailed), Message: fmt.Sprintf("failed to submit request: %v", err)},
		})
		d.progress.record(ctx, false)
		metrics.RecordRequestError(modelID)
		return
	}

	// Register in-flight and start per-request timeout watchdog.
	// The timer and collectResults race to claim the inflight entry via delete.
	// Whoever deletes it first processes the entry; the other sees !ok and no-ops.
	// Cap timeout at SLO deadline to avoid exceeding job SLO.
	timeout := d.perRequestTimeout
	if dl, ok := sloCtx.Deadline(); ok {
		remaining := time.Until(dl)
		if remaining < timeout {
			timeout = remaining
		}
	}
	timer := time.AfterFunc(timeout, func() {
		d.mu.Lock()
		pending, ok := d.inflight[batchReqID]
		if ok {
			delete(d.inflight, batchReqID)
			d.poolCounts[pending.poolName]--
		}
		d.mu.Unlock()
		if !ok {
			return
		}
		d.logger.V(logging.DEBUG).Info("Per-request timeout fired", "id", batchReqID, "customId", pending.customID)
		d.writeErrorLine(ctx, &outputLine{
			ID:       batchReqID,
			CustomID: pending.customID,
			Error:    &outputError{Code: string(batch_types.ErrCodeBatchFailed), Message: "per-request timeout exceeded"},
		})
		d.progress.record(ctx, false)
		metrics.RecordRequestError(pending.modelID)
	})

	d.mu.Lock()
	d.inflight[batchReqID] = &pendingRequest{
		customID: req.CustomID,
		poolName: poolName,
		modelID:  modelID,
		timer:    timer,
	}
	d.poolCounts[poolName]++
	d.mu.Unlock()
}

// collectResults polls GetResult for the given pool until all in-flight results
// for that pool are received or the context is cancelled.
func (d *asyncDispatcher) collectResults(ctx context.Context, poolName string) {
	defer d.collectWg.Done()

	prod, ok := d.producers[poolName]
	if !ok {
		d.logger.Error(fmt.Errorf("producer not found for pool %q", poolName), "Collector cannot start")
		return
	}

	logger := d.logger.WithValues("pool", poolName)
	submissionsDone := false
	var backstopTimer *time.Timer
	var backstopCh <-chan time.Time

	for {
		if ctx.Err() != nil {
			break
		}

		// Once submissions are complete and there are no more in-flight entries
		// for this pool, the collector is done.
		if !submissionsDone {
			select {
			case <-d.submissionsDone:
				submissionsDone = true
				// Start backstop timer to prevent hanging on lost results.
				backstopTimer = time.NewTimer(collectorBackstopTimeout)
				backstopCh = backstopTimer.C
				logger.V(logging.INFO).Info("Submissions done, started backstop timer", "timeout", collectorBackstopTimeout)
			default:
			}
		}
		if submissionsDone && d.inflightCountForPool(poolName) == 0 {
			if backstopTimer != nil {
				backstopTimer.Stop()
			}
			return
		}

		// Check backstop timeout — force exit if we've waited too long.
		if backstopCh != nil {
			select {
			case <-backstopCh:
				logger.V(logging.INFO).Info("Backstop timeout expired, draining remaining in-flight entries",
					"remaining", d.inflightCountForPool(poolName))
				d.drainInflightForPool(ctx, poolName)
				return
			default:
			}
		}

		pollCtx, cancel := context.WithTimeout(ctx, d.pollTimeout)
		result, err := prod.GetResult(pollCtx)
		cancel()

		if err != nil {
			if ctx.Err() != nil {
				break
			}
			// Poll cycle timed out with no result — loop and try again.
			if !errors.Is(err, context.DeadlineExceeded) {
				logger.Error(err, "Unexpected error from GetResult")
			}
			continue
		}

		d.handleResult(ctx, result)
	}

	// Context cancelled: drain remaining in-flight entries for this pool.
	d.drainInflightForPool(ctx, poolName)
}

// handleResult processes a single ResultMessage, writing the output line and
// recording progress.
func (d *asyncDispatcher) handleResult(ctx context.Context, msg *asyncapi.ResultMessage) {
	d.mu.Lock()
	pending, ok := d.inflight[msg.ID]
	if ok {
		delete(d.inflight, msg.ID)
		d.poolCounts[pending.poolName]--
	}
	d.mu.Unlock()

	if !ok {
		// Timer already fired for this entry, or stale result from a previous run.
		d.logger.V(logging.DEBUG).Info("Received result for unknown/timed-out request, discarding", "id", msg.ID)
		return
	}

	pending.timer.Stop()

	line := d.buildOutputLine(msg.ID, pending.customID, msg.Payload)
	d.progress.record(ctx, line.isSuccess())

	lineBytes, err := json.Marshal(line)
	if err != nil {
		d.logger.Error(err, "Failed to marshal result line", "id", msg.ID)
		return
	}
	lineBytes = append(lineBytes, '\n')

	isError := line.Error != nil
	if writeErr := d.writers.write(lineBytes, isError); writeErr != nil {
		d.logger.Error(writeErr, "Failed to write result line", "id", msg.ID)
	}
	if !line.isSuccess() {
		metrics.RecordRequestError(pending.modelID)
	}
}

// buildOutputLine parses a ResultMessage payload string into an outputLine.
// An empty payload or a payload with an "error" key produces an error line.
func (d *asyncDispatcher) buildOutputLine(id, customID, payload string) *outputLine {
	line := &outputLine{ID: id, CustomID: customID}

	if payload == "" {
		line.Error = &outputError{
			Code:    string(batch_types.ErrCodeBatchFailed),
			Message: "async result payload is empty",
		}
		return line
	}

	var body map[string]interface{}
	if err := json.Unmarshal([]byte(payload), &body); err != nil {
		d.logger.Error(err, "Failed to unmarshal async result payload", "id", id)
		line.Error = &outputError{
			Code:    string(batch_types.ErrCodeBatchFailed),
			Message: fmt.Sprintf("failed to parse result payload: %v", err),
		}
		return line
	}

	// A payload with an "error" field signals a failed inference.
	if errVal, hasErr := body["error"]; hasErr {
		line.Error = &outputError{
			Code:    string(batch_types.ErrCodeBatchFailed),
			Message: fmt.Sprintf("%v", errVal),
		}
		return line
	}

	line.Response = &batch_types.ResponseData{
		StatusCode: 200,
		RequestID:  id,
		Body:       body,
	}
	return line
}

// drainInflightForPool stops all pending timers and writes error lines for any
// remaining in-flight entries belonging to poolName. Called when the context is
// cancelled (SLO expiry, user cancel, or system error).
func (d *asyncDispatcher) drainInflightForPool(ctx context.Context, poolName string) {
	d.mu.Lock()
	var ids []string
	var pending []*pendingRequest
	for id, p := range d.inflight {
		if p.poolName == poolName {
			ids = append(ids, id)
			pending = append(pending, p)
		}
	}
	for _, id := range ids {
		delete(d.inflight, id)
	}
	d.poolCounts[poolName] -= len(ids)
	d.mu.Unlock()

	if len(pending) == 0 {
		return
	}

	var errCode batch_types.BatchErrorCode
	switch {
	case errors.Is(d.sloCtx.Err(), context.DeadlineExceeded):
		errCode = batch_types.ErrCodeBatchExpired
	case d.userCancelCtx.Err() != nil:
		errCode = batch_types.ErrCodeBatchCancelled
	default:
		errCode = batch_types.ErrCodeBatchFailed
	}

	for i, p := range pending {
		p.timer.Stop()
		d.writeErrorLine(ctx, &outputLine{
			ID:       ids[i],
			CustomID: p.customID,
			Error:    &outputError{Code: string(errCode), Message: errCode.Message()},
		})
		d.progress.record(ctx, false)
	}
}

// inflightCountForPool returns the number of in-flight entries for poolName.
func (d *asyncDispatcher) inflightCountForPool(poolName string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.poolCounts[poolName]
}

// writeErrorLine marshals line and appends it to the error file.
func (d *asyncDispatcher) writeErrorLine(ctx context.Context, line *outputLine) {
	lineBytes, err := json.Marshal(line)
	if err != nil {
		d.logger.Error(err, "Failed to marshal error line", "id", line.ID)
		return
	}
	lineBytes = append(lineBytes, '\n')
	if writeErr := d.writers.write(lineBytes, true); writeErr != nil {
		d.logger.Error(writeErr, "Failed to write error line", "id", line.ID)
	}
}
