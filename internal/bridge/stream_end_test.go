package bridge

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestSSEStreamEndContract(t *testing.T) {
	choice := func(index int, delta map[string]any, finish any) map[string]any {
		ch := map[string]any{"index": index, "delta": delta}
		if finish != nil {
			ch["finish_reason"] = finish
		}
		return ch
	}
	frame := func(choices ...map[string]any) []byte {
		return sseFrame(map[string]any{"choices": choices})
	}
	tool := func(id, name, args string) []any {
		return []any{map[string]any{"index": 0, "id": id, "type": "function", "function": map[string]any{"name": name, "arguments": args}}}
	}
	content := frame(choice(0, map[string]any{"content": "hello"}, nil))
	finished := frame(choice(0, map[string]any{}, "stop"))
	usage := sseFrame(map[string]any{"choices": []any{}, "usage": map[string]any{"prompt_tokens": 11, "completion_tokens": 3}})
	done := []byte("data: [DONE]\r\n\r\n")

	tests := []struct {
		name            string
		parts           [][]byte
		plan            hostPlan
		wantEnd         string
		wantError       string
		wantChoices     int
		wantUsage       bool
		expectedChoices int64
	}{
		{name: "text EOF", parts: [][]byte{content, finished}, wantEnd: "eof_after_finish", wantChoices: 1},
		{name: "fragmented tool EOF", parts: [][]byte{
			frame(choice(0, map[string]any{"tool_calls": tool("call-1", "lookup", `{"x":`)}, nil)),
			frame(choice(0, map[string]any{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"arguments": "1}"}}}}, "tool_calls")),
		}, wantEnd: "eof_after_finish", wantChoices: 1},
		{name: "legacy function EOF", parts: [][]byte{
			frame(choice(0, map[string]any{"function_call": map[string]any{"name": "lookup", "arguments": `{"x":`}}, nil)),
			frame(choice(0, map[string]any{"function_call": map[string]any{"arguments": "1}"}}, "function_call")),
		}, wantEnd: "eof_after_finish", wantChoices: 1},
		{name: "all choices finished EOF", parts: [][]byte{frame(
			choice(0, map[string]any{"content": "first"}, "stop"),
			choice(1, map[string]any{"content": "second"}, "stop"),
		)}, wantEnd: "eof_after_finish", wantChoices: 2, expectedChoices: 2},
		{name: "missing requested second choice", parts: [][]byte{content, finished}, expectedChoices: 2},
		{name: "wrong choice index", parts: [][]byte{frame(choice(1, map[string]any{"content": "hello"}, "stop"))}},
		{name: "usage after finish EOF", parts: [][]byte{content, finished, usage}, wantEnd: "eof_after_finish", wantChoices: 1, wantUsage: true},
		{name: "normal DONE", parts: [][]byte{content, finished, done}, wantEnd: "done", wantChoices: 1},
		{name: "partial frame after finish", parts: [][]byte{content, finished, []byte("data: {\"choices\":")}},
		{name: "missing finish", parts: [][]byte{content}},
		{name: "empty finish", parts: [][]byte{content, frame(choice(0, map[string]any{}, ""))}},
		{name: "no output", parts: [][]byte{finished}},
		{name: "malformed tool arguments", parts: [][]byte{frame(choice(0, map[string]any{"tool_calls": tool("call-1", "lookup", "{")}, "tool_calls"))}},
		{name: "array tool arguments", parts: [][]byte{frame(choice(0, map[string]any{"tool_calls": tool("call-1", "lookup", "[]")}, "tool_calls"))}},
		{name: "missing tool arguments", parts: [][]byte{frame(choice(0, map[string]any{"tool_calls": tool("call-1", "lookup", "")}, "tool_calls"))}},
		{name: "missing tool ID", parts: [][]byte{frame(choice(0, map[string]any{"tool_calls": tool("", "lookup", "{}")}, "tool_calls"))}},
		{name: "missing tool name", parts: [][]byte{frame(choice(0, map[string]any{"tool_calls": tool("call-1", "", "{}")}, "tool_calls"))}},
		{name: "tool finish without tool", parts: [][]byte{content, frame(choice(0, map[string]any{}, "tool_calls"))}},
		{name: "legacy finish without function", parts: [][]byte{content, frame(choice(0, map[string]any{}, "function_call"))}},
		{name: "legacy missing name", parts: [][]byte{frame(choice(0, map[string]any{"function_call": map[string]any{"arguments": "{}"}}, "function_call"))}},
		{name: "legacy array arguments", parts: [][]byte{frame(choice(0, map[string]any{"function_call": map[string]any{"name": "lookup", "arguments": "[]"}}, "function_call"))}},
		{name: "unfinished second choice", parts: [][]byte{frame(
			choice(0, map[string]any{"content": "first"}, "stop"),
			choice(1, map[string]any{"content": "second"}, nil),
		)}},
		{name: "explicit error after finish", parts: [][]byte{content, finished, sseFrame(map[string]any{"error": map[string]any{"message": "quota exhausted"}})}, wantError: "quota exhausted"},
		{name: "transport error after finish", plan: hostPlan{
			status: 200, header: http.Header{"Content-Type": []string{"text/event-stream"}},
			chunks: []readChunk{{Payload: content}, {Payload: finished}, {Error: "connection reset", Done: true}},
		}, wantError: "connection reset"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := ssePlan(tt.parts...)
			if tt.plan.chunks != nil {
				plan = tt.plan
			}
			s := registeredService(t, "stream-aggregate")
			h := newFakeHost()
			h.streams["upstream-1"] = plan.chunks
			s.SetHost(h.call)
			entry := LogEntry{}
			attempt := Attempt{}
			cp, err := s.consumeSSE(upstreamStream{
				StatusCode: plan.status, Headers: plan.header, StreamID: "upstream-1",
			}, "deepseek-flash", &entry, &attempt, time.Now(), nil, tt.expectedChoices)
			if tt.wantEnd == "" {
				if err == nil || statusOf(err) != 502 || cp.done || entry.StreamEnd == "eof_after_finish" {
					t.Fatalf("incomplete stream: completion=%#v end=%q error=%v", cp, entry.StreamEnd, err)
				}
				if tt.wantError != "" && !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("stream error = %v, want %q", err, tt.wantError)
				}
				return
			}
			if err != nil || !cp.done || entry.StreamEnd != tt.wantEnd {
				t.Fatalf("complete stream: done=%t end=%q error=%v, want %q", cp.done, entry.StreamEnd, err, tt.wantEnd)
			}
			body, err := cp.result("deepseek-flash")
			if err != nil {
				t.Fatalf("aggregate completed stream: %v", err)
			}
			var result map[string]any
			if err := json.Unmarshal(body, &result); err != nil || len(list(result["choices"])) != tt.wantChoices {
				t.Fatalf("completed choices = %s, error = %v", body, err)
			}
			if tt.wantUsage && (number(object(result["usage"])["prompt_tokens"]) != 11 || entry.PromptTokens != 11 || entry.CompletionTokens != 3) {
				t.Fatalf("late usage lost: body=%s entry=%#v", body, entry)
			}
		})
	}
}

func TestEOFUsageReachesAggregateAndStreamingExecutors(t *testing.T) {
	content := sseFrame(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "hello"}}}})
	finish := sseFrame(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}})
	usage := sseFrame(map[string]any{"choices": []any{}, "usage": map[string]any{"prompt_tokens": 11, "completion_tokens": 3, "prompt_tokens_details": map[string]any{"cached_tokens": 5}}})

	for _, stream := range []bool{false, true} {
		name := "aggregate"
		if stream {
			name = "streaming"
		}
		t.Run(name, func(t *testing.T) {
			s := registeredService(t, "stream-aggregate")
			h := newFakeHost(ssePlan(content, finish, usage))
			s.SetHost(h.call)
			if stream {
				if _, err := s.Handle("executor.execute_stream", executorRequest("client-1")); err != nil {
					t.Fatalf("start stream: %v", err)
				}
				select {
				case <-h.clientClosed:
				case <-time.After(3 * time.Second):
					t.Fatal("plugin did not close client stream")
				}
				h.mu.Lock()
				emitted := append([][]byte(nil), h.emitted...)
				closeError := h.clientError
				h.mu.Unlock()
				if closeError != "" || len(emitted) != 3 {
					t.Fatalf("stream output count=%d close error=%q", len(emitted), closeError)
				}
				var last map[string]any
				if err := json.Unmarshal(emitted[2], &last); err != nil || number(object(last["usage"])["prompt_tokens"]) != 11 {
					t.Fatalf("late usage chunk = %s, error = %v", emitted[2], err)
				}
			} else {
				result, err := s.Handle("executor.execute", executorRequest(""))
				if err != nil {
					t.Fatalf("aggregate EOF stream: %v", err)
				}
				var body map[string]any
				if err := json.Unmarshal(result.(Response).Payload, &body); err != nil || number(object(body["usage"])["prompt_tokens"]) != 11 {
					t.Fatalf("aggregate usage = %s, error = %v", result.(Response).Payload, err)
				}
			}
			if len(s.logs) != 1 || s.logs[0].Status != 200 || s.logs[0].StreamEnd != "eof_after_finish" || !s.logs[0].UsageReported || s.logs[0].PromptTokens != 11 || s.logs[0].CompletionTokens != 3 || s.logs[0].CachedTokens != 5 {
				t.Fatalf("executor log lost stream end or late usage: %#v", s.logs)
			}
		})
	}
}
