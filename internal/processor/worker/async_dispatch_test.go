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
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	asyncapi "github.com/llm-d-incubation/llm-d-async/api"
	asyncprod "github.com/llm-d-incubation/llm-d-async/producer"

	"github.com/llm-d-incubation/batch-gateway/internal/processor/config"
	batch_types "github.com/llm-d-incubation/batch-gateway/internal/shared/types"
)

var _ asyncprod.Producer = (*mockAsyncProducer)(nil)

// mockAsyncProducer implements producer.Producer for unit tests.
type mockAsyncProducer struct {
	mu sync.Mutex

	submitFn    func(ctx context.Context, req asyncapi.Request) error
	getResultFn func(ctx context.Context) (*asyncapi.ResultMessage, error)
	submitted   []*asyncapi.RequestMessage
}

func (m *mockAsyncProducer) SubmitRequest(ctx context.Context, req asyncapi.Request) error {
	if m.submitFn != nil {
		return m.submitFn(ctx, req)
	}
	msg, ok := req.(*asyncapi.RequestMessage)
	if !ok {
		return fmt.Errorf("unexpected request type %T", req)
	}
	m.mu.Lock()
	m.submitted = append(m.submitted, msg)
	m.mu.Unlock()
	return nil
}

func (m *mockAsyncProducer) GetResult(ctx context.Context) (*asyncapi.ResultMessage, error) {
	if m.getResultFn != nil {
		return m.getResultFn(ctx)
	}
	return nil, context.DeadlineExceeded
}

func (m *mockAsyncProducer) Close() error { return nil }

func (m *mockAsyncProducer) lastSubmitted() *asyncapi.RequestMessage {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.submitted) == 0 {
		return nil
	}
	return m.submitted[len(m.submitted)-1]
}

func (m *mockAsyncProducer) submittedCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.submitted)
}

// chanAsyncProducer delivers a result per SubmitRequest via a buffered channel.
type chanAsyncProducer struct {
	mockAsyncProducer
	results chan *asyncapi.ResultMessage
}

func newChanAsyncProducer(buffer int) *chanAsyncProducer {
	p := &chanAsyncProducer{results: make(chan *asyncapi.ResultMessage, buffer)}
	p.getResultFn = func(ctx context.Context) (*asyncapi.ResultMessage, error) {
		select {
		case r := <-p.results:
			return r, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	p.submitFn = func(_ context.Context, req asyncapi.Request) error {
		msg, ok := req.(*asyncapi.RequestMessage)
		if !ok {
			return fmt.Errorf("unexpected request type %T", req)
		}
		p.mu.Lock()
		p.submitted = append(p.submitted, msg)
		p.mu.Unlock()

		payload, err := json.Marshal(map[string]interface{}{
			"choices": []map[string]string{{"message": "ok"}},
		})
		if err != nil {
			return err
		}
		select {
		case p.results <- &asyncapi.ResultMessage{ID: msg.ID, Payload: string(payload)}:
		default:
			return errors.New("result channel full")
		}
		return nil
	}
	return p
}

func asyncTestConfig(t *testing.T) *config.ProcessorConfig {
	t.Helper()
	cfg := config.NewConfig()
	cfg.WorkDir = t.TempDir()
	cfg.DispatchMode = config.DispatchModeAsync
	cfg.AsyncDispatchConfig.ResultPollTimeout = 50 * time.Millisecond
	cfg.ModelGateways = map[string]config.ModelGatewayConfig{
		"m1": {
			URL:               "http://ignored:8000",
			InferencePoolName: "pool-a",
		},
	}
	return cfg
}

type asyncTestBuffers struct {
	output *bytes.Buffer
	errors *bytes.Buffer
}

func newTestAsyncDispatcher(
	t *testing.T,
	producers map[string]asyncprod.Producer,
	total int64,
	sloCtx, userCancelCtx context.Context,
) (*asyncDispatcher, *outputWriters, *executionProgress, *asyncTestBuffers) {
	t.Helper()

	origInterval := progressUpdateInterval
	progressUpdateInterval = time.Hour
	t.Cleanup(func() { progressUpdateInterval = origInterval })

	cfg := asyncTestConfig(t)
	buffers := &asyncTestBuffers{
		output: &bytes.Buffer{},
		errors: &bytes.Buffer{},
	}
	writers := &outputWriters{
		output: bufio.NewWriter(buffers.output),
		errors: bufio.NewWriter(buffers.errors),
	}
	progress := &executionProgress{total: total, jobID: "job-1"}
	progress.lastUpdate.Store(time.Now().UnixNano())

	d := newAsyncDispatcher(
		producers,
		writers,
		progress,
		cfg,
		"job-1",
		"tenant-1",
		sloCtx,
		userCancelCtx,
		testLogger(t),
	)
	return d, writers, progress, buffers
}

func openAsyncInputFile(t *testing.T, requests []batch_types.Request) (*os.File, []planEntry) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "input.jsonl")
	raw := writeInputJSONL(t, path, requests)
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open input: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f, planEntriesFromLines(raw)
}

func flushAsyncWriters(t *testing.T, writers *outputWriters) {
	t.Helper()
	writers.outputMu.Lock()
	err1 := writers.output.Flush()
	writers.outputMu.Unlock()
	if err1 != nil {
		t.Fatalf("flush output: %v", err1)
	}
	writers.errorsMu.Lock()
	err2 := writers.errors.Flush()
	writers.errorsMu.Unlock()
	if err2 != nil {
		t.Fatalf("flush errors: %v", err2)
	}
}

func TestAsyncDispatcher_buildOutputLine(t *testing.T) {
	d := &asyncDispatcher{logger: testLogger(t)}

	tests := []struct {
		name       string
		payload    string
		wantError  bool
		wantStatus int
	}{
		{
			name:       "success payload",
			payload:    `{"choices":[{"message":{"content":"hi"}}]}`,
			wantStatus: 200,
		},
		{
			name:      "empty payload",
			wantError: true,
		},
		{
			name:      "error field in payload",
			payload:   `{"error":"inference failed"}`,
			wantError: true,
		},
		{
			name:      "invalid json",
			payload:   `{not-json`,
			wantError: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			line := d.buildOutputLine("batch_req_1", "custom-1", tc.payload)
			if line.CustomID != "custom-1" {
				t.Fatalf("CustomID = %q, want custom-1", line.CustomID)
			}
			if tc.wantError {
				if line.Error == nil {
					t.Fatal("expected error line, got success")
				}
				return
			}
			if line.Error != nil {
				t.Fatalf("unexpected error: %+v", line.Error)
			}
			if line.Response == nil || line.Response.StatusCode != tc.wantStatus {
				t.Fatalf("Response = %+v, want status %d", line.Response, tc.wantStatus)
			}
		})
	}
}

func TestAsyncDispatcher_submitRequest_success(t *testing.T) {
	prod := &mockAsyncProducer{}
	d, _, progress, _ := newTestAsyncDispatcher(t, map[string]asyncprod.Producer{"pool-a": prod}, 1, context.Background(), context.Background()) //nolint:dogsled

	reqs := []batch_types.Request{
		{CustomID: "req-1", Method: "POST", URL: "/v1/embeddings", Body: map[string]interface{}{"model": "m1", "input": "hi"}},
	}
	inputFile, entries := openAsyncInputFile(t, reqs)

	sloCtx, sloCancel := context.WithDeadline(context.Background(), time.Now().Add(time.Hour))
	defer sloCancel()

	d.submitRequest(testLoggerCtx(t), sloCtx, inputFile, entries[0], "m1", map[string]string{"x-trace": "abc"})

	submitted := prod.lastSubmitted()
	if submitted == nil {
		t.Fatal("expected SubmitRequest to be called")
	}
	if submitted.Endpoint != "/v1/embeddings" {
		t.Fatalf("Endpoint = %q, want /v1/embeddings", submitted.Endpoint)
	}
	if submitted.Metadata["job_id"] != "job-1" {
		t.Fatalf("Metadata[job_id] = %q, want job-1", submitted.Metadata["job_id"])
	}
	if submitted.Headers["x-trace"] != "abc" {
		t.Fatalf("Headers[x-trace] = %q, want abc", submitted.Headers["x-trace"])
	}
	if d.inflightCountForPool("pool-a") != 1 {
		t.Fatalf("inflight = %d, want 1", d.inflightCountForPool("pool-a"))
	}
	counts := progress.counts()
	if counts.Completed != 0 && counts.Failed != 0 {
		t.Fatalf("unexpected progress after submit: completed=%d failed=%d", counts.Completed, counts.Failed)
	}
}

func TestAsyncDispatcher_submitRequest_errors(t *testing.T) {
	tests := []struct {
		name          string
		mutate        func(t *testing.T, d *asyncDispatcher, prod *mockAsyncProducer)
		modelID       string
		wantErrSubstr string
	}{
		{
			name: "model_not_found when pool not configured",
			mutate: func(t *testing.T, d *asyncDispatcher, _ *mockAsyncProducer) {
				d.cfg.ModelGateways = nil
			},
			modelID:       "unknown-model",
			wantErrSubstr: "model_not_found",
		},
		{
			name: "submit failure",
			mutate: func(_ *testing.T, _ *asyncDispatcher, prod *mockAsyncProducer) {
				prod.submitFn = func(context.Context, asyncapi.Request) error {
					return errors.New("redis down")
				}
			},
			modelID:       "m1",
			wantErrSubstr: "failed to submit request",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			prod := &mockAsyncProducer{}
			d, writers, progress, bufs := newTestAsyncDispatcher(t, map[string]asyncprod.Producer{"pool-a": prod}, 1, context.Background(), context.Background())
			tc.mutate(t, d, prod)

			reqs := []batch_types.Request{
				{CustomID: "req-1", Method: "POST", URL: "/v1/chat/completions", Body: map[string]interface{}{"model": "m1"}},
			}
			inputFile, entries := openAsyncInputFile(t, reqs)

			d.submitRequest(testLoggerCtx(t), context.Background(), inputFile, entries[0], tc.modelID, nil)
			flushAsyncWriters(t, writers)

			errLines := bytes.TrimSpace(bufs.errors.Bytes())
			if len(errLines) == 0 {
				t.Fatal("expected error output line")
			}
			if !bytes.Contains(errLines, []byte(tc.wantErrSubstr)) {
				t.Fatalf("error line %q does not contain %q", errLines, tc.wantErrSubstr)
			}
			counts := progress.counts()
			if counts.Failed != 1 {
				t.Fatalf("failed count = %d, want 1", counts.Failed)
			}
		})
	}
}

func TestAsyncDispatcher_submitRequest_parseError(t *testing.T) {
	prod := &mockAsyncProducer{}
	d, writers, progress, bufs := newTestAsyncDispatcher(t, map[string]asyncprod.Producer{"pool-a": prod}, 1, context.Background(), context.Background())

	path := filepath.Join(t.TempDir(), "bad.jsonl")
	if err := os.WriteFile(path, []byte("not-json\n"), 0o644); err != nil {
		t.Fatalf("write bad input: %v", err)
	}
	inputFile, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer inputFile.Close()

	d.submitRequest(testLoggerCtx(t), context.Background(), inputFile, planEntry{Offset: 0, Length: 9}, "m1", nil)
	flushAsyncWriters(t, writers)

	if !bytes.Contains(bufs.errors.Bytes(), []byte("failed to parse request")) {
		t.Fatalf("error line %q does not contain parse error", bufs.errors.Bytes())
	}
	if progress.counts().Failed != 1 {
		t.Fatalf("failed = %d, want 1", progress.counts().Failed)
	}
}

func TestAsyncDispatcher_handleResult(t *testing.T) {
	t.Run("writes success and records progress", func(t *testing.T) {
		d, writers, progress, bufs := newTestAsyncDispatcher(t, nil, 1, context.Background(), context.Background())
		batchReqID := newBatchRequestID("srv-1")
		d.mu.Lock()
		d.inflight[batchReqID] = &pendingRequest{customID: "custom-1", poolName: "pool-a", modelID: "m1", timer: time.AfterFunc(time.Hour, func() {})}
		d.poolCounts["pool-a"]++
		d.mu.Unlock()

		payload, _ := json.Marshal(map[string]interface{}{"ok": true})
		d.handleResult(testLoggerCtx(t), &asyncapi.ResultMessage{ID: batchReqID, Payload: string(payload)})
		flushAsyncWriters(t, writers)

		if d.inflightCountForPool("pool-a") != 0 {
			t.Fatalf("inflight = %d, want 0", d.inflightCountForPool("pool-a"))
		}
		counts := progress.counts()
		if counts.Completed != 1 {
			t.Fatalf("completed = %d, want 1", counts.Completed)
		}
		if len(bytes.TrimSpace(bufs.output.Bytes())) == 0 {
			t.Fatal("expected output line")
		}
	})

	t.Run("discards stale result", func(t *testing.T) {
		d, writers, progress, bufs := newTestAsyncDispatcher(t, nil, 1, context.Background(), context.Background())
		d.handleResult(testLoggerCtx(t), &asyncapi.ResultMessage{ID: "batch_req_missing", Payload: `{"ok":true}`})
		flushAsyncWriters(t, writers)

		counts := progress.counts()
		if counts.Completed != 0 || counts.Failed != 0 {
			t.Fatalf("progress changed for stale result: completed=%d failed=%d", counts.Completed, counts.Failed)
		}
		if len(bufs.output.Bytes()) != 0 {
			t.Fatal("expected no output for stale result")
		}
	})
}

func TestAsyncDispatcher_perRequestTimeout(t *testing.T) {
	prod := &mockAsyncProducer{}
	d, writers, progress, bufs := newTestAsyncDispatcher(t, map[string]asyncprod.Producer{"pool-a": prod}, 1, context.Background(), context.Background())
	d.perRequestTimeout = 30 * time.Millisecond

	reqs := []batch_types.Request{
		{CustomID: "req-timeout", Method: "POST", URL: "/v1/chat/completions", Body: map[string]interface{}{"model": "m1"}},
	}
	inputFile, entries := openAsyncInputFile(t, reqs)
	d.submitRequest(testLoggerCtx(t), context.Background(), inputFile, entries[0], "m1", nil)

	// Wait for timeout to fire and inflight to be cleared (indicates timer callback finished)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if d.inflightCountForPool("pool-a") == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Give a small buffer for any final writes to complete
	time.Sleep(20 * time.Millisecond)
	flushAsyncWriters(t, writers)

	if !bytes.Contains(bufs.errors.Bytes(), []byte("per-request timeout exceeded")) {
		t.Fatalf("expected timeout error line, got: %s", bufs.errors.Bytes())
	}
	if progress.counts().Failed != 1 {
		t.Fatalf("failed = %d, want 1", progress.counts().Failed)
	}
	if d.inflightCountForPool("pool-a") != 0 {
		t.Fatalf("inflight = %d, want 0 after timeout", d.inflightCountForPool("pool-a"))
	}
}

func TestAsyncDispatcher_drainInflightForPool(t *testing.T) {
	tests := []struct {
		name     string
		sloCtx   context.Context
		userCtx  context.Context
		wantCode string
	}{
		{
			name: "batch_expired on SLO deadline",
			sloCtx: func() context.Context {
				ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				cancel()
				return ctx
			}(),
			userCtx:  context.Background(),
			wantCode: string(batch_types.ErrCodeBatchExpired),
		},
		{
			name:     "batch_cancelled on user cancel",
			sloCtx:   context.Background(),
			userCtx:  func() context.Context { ctx, cancel := context.WithCancel(context.Background()); cancel(); return ctx }(),
			wantCode: string(batch_types.ErrCodeBatchCancelled),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, writers, progress, bufs := newTestAsyncDispatcher(t, nil, 1, tc.sloCtx, tc.userCtx)
			batchReqID := newBatchRequestID("drain-1")
			d.mu.Lock()
			d.inflight[batchReqID] = &pendingRequest{
				customID: "custom-drain",
				poolName: "pool-a",
				modelID:  "m1",
				timer:    time.AfterFunc(time.Hour, func() {}),
			}
			d.poolCounts["pool-a"]++
			d.mu.Unlock()

			d.drainInflightForPool(testLoggerCtx(t), "pool-a")
			flushAsyncWriters(t, writers)

			if !bytes.Contains(bufs.errors.Bytes(), []byte(tc.wantCode)) {
				t.Fatalf("error line %q does not contain %q", bufs.errors.Bytes(), tc.wantCode)
			}
			if d.inflightCountForPool("pool-a") != 0 {
				t.Fatalf("inflight = %d, want 0", d.inflightCountForPool("pool-a"))
			}
			counts := progress.counts()
			if counts.Failed != 1 {
				t.Fatalf("failed = %d, want 1", counts.Failed)
			}
		})
	}
}

// TestAsyncDispatcher_SubmitCollectCycle verifies submit → collector → output for one pool.
func TestAsyncDispatcher_SubmitCollectCycle(t *testing.T) {
	prod := newChanAsyncProducer(4)
	d, writers, progress, bufs := newTestAsyncDispatcher(t, map[string]asyncprod.Producer{"pool-a": prod}, 2, context.Background(), context.Background())

	reqs := []batch_types.Request{
		{CustomID: "a", Method: "POST", URL: "/v1/chat/completions", Body: map[string]interface{}{"model": "m1"}},
		{CustomID: "b", Method: "POST", URL: "/v1/chat/completions", Body: map[string]interface{}{"model": "m1"}},
	}
	inputFile, entries := openAsyncInputFile(t, reqs)

	ctx, cancel := context.WithCancel(testLoggerCtx(t))
	defer cancel()

	d.startCollectors(ctx, []string{"pool-a"})
	for _, entry := range entries {
		d.submitRequest(ctx, context.Background(), inputFile, entry, "m1", nil)
	}
	d.signalSubmissionsDone()

	select {
	case <-d.collectDone:
	case <-time.After(5 * time.Second):
		t.Fatal("collectDone not closed within timeout")
	}
	flushAsyncWriters(t, writers)

	if prod.submittedCount() != 2 {
		t.Fatalf("submitted = %d, want 2", prod.submittedCount())
	}
	counts := progress.counts()
	if counts.Completed != 2 {
		t.Fatalf("completed = %d, want 2", counts.Completed)
	}
	lines := bytes.Split(bytes.TrimSpace(bufs.output.Bytes()), []byte{'\n'})
	if len(lines) != 2 {
		t.Fatalf("output lines = %d, want 2; body=%s", len(lines), bufs.output.Bytes())
	}
}
