package commandbridge

import (
	"encoding/json"
	"fmt"
	"strings"
)

type StreamParser interface {
	Parse([]byte) ([]StreamEvent, error)
	Flush() ([]StreamEvent, error)
	Transcript() string
}

type StreamEvent struct {
	Update map[string]any
}

type JSONLMapping struct {
	Events         []StreamEvent
	TranscriptText string
}

type JSONLMapper func(map[string]any) (JSONLMapping, error)

type JSONLStreamParser struct {
	mapper     JSONLMapper
	buffer     []byte
	transcript strings.Builder
}

func NewJSONLStreamParser(mapper JSONLMapper) *JSONLStreamParser {
	return &JSONLStreamParser{mapper: mapper}
}

func (p *JSONLStreamParser) Parse(chunk []byte) ([]StreamEvent, error) {
	if p == nil || len(chunk) == 0 {
		return nil, nil
	}
	p.buffer = append(p.buffer, chunk...)
	var out []StreamEvent
	for {
		idx := -1
		for i, b := range p.buffer {
			if b == '\n' {
				idx = i
				break
			}
		}
		if idx < 0 {
			return out, nil
		}
		line := append([]byte(nil), p.buffer[:idx]...)
		p.buffer = p.buffer[idx+1:]
		events, err := p.mapLine(line)
		if err != nil {
			return out, err
		}
		out = append(out, events...)
	}
}

func (p *JSONLStreamParser) Flush() ([]StreamEvent, error) {
	if p == nil || len(p.buffer) == 0 {
		return nil, nil
	}
	line := append([]byte(nil), p.buffer...)
	p.buffer = nil
	return p.mapLine(line)
}

func (p *JSONLStreamParser) Transcript() string {
	if p == nil {
		return ""
	}
	return p.transcript.String()
}

func (p *JSONLStreamParser) mapLine(line []byte) ([]StreamEvent, error) {
	if strings.TrimSpace(string(line)) == "" {
		return nil, nil
	}
	var value map[string]any
	if err := json.Unmarshal(line, &value); err != nil {
		return nil, fmt.Errorf("parse JSONL stream event: %w", err)
	}
	if p.mapper == nil {
		return nil, nil
	}
	mapping, err := p.mapper(value)
	if err != nil {
		return nil, err
	}
	if mapping.TranscriptText != "" {
		p.transcript.WriteString(mapping.TranscriptText)
	}
	return mapping.Events, nil
}

func AgentMessageChunk(text string) StreamEvent {
	return textChunk("agent_message_chunk", "", text)
}

func AgentThoughtChunk(messageID, text string) StreamEvent {
	return textChunk("agent_thought_chunk", messageID, text)
}

func ToolCallStart(id, title, kind, status string, rawInput any) StreamEvent {
	update := map[string]any{
		"sessionUpdate": "tool_call",
		"toolCallId":    id,
		"title":         firstNonEmpty(title, id),
	}
	if kind != "" {
		update["kind"] = kind
	}
	if status != "" {
		update["status"] = status
	}
	if rawInput != nil {
		update["rawInput"] = rawInput
	}
	return StreamEvent{Update: update}
}

func ToolCallFinish(id, title, kind, status string, rawOutput any) StreamEvent {
	update := map[string]any{
		"sessionUpdate": "tool_call_update",
		"toolCallId":    id,
	}
	if title != "" {
		update["title"] = title
	}
	if kind != "" {
		update["kind"] = kind
	}
	if status != "" {
		update["status"] = status
	}
	if rawOutput != nil {
		update["rawOutput"] = rawOutput
		update["content"] = []map[string]any{{
			"type": "content",
			"content": map[string]any{
				"type": "text",
				"text": stringifyStreamValue(rawOutput),
			},
		}}
	}
	return StreamEvent{Update: update}
}

func UsageUpdate(used, size int) StreamEvent {
	return StreamEvent{Update: map[string]any{
		"sessionUpdate": "usage_update",
		"used":          used,
		"size":          size,
	}}
}

func textChunk(updateType, messageID, text string) StreamEvent {
	update := map[string]any{
		"sessionUpdate": updateType,
		"content": map[string]any{
			"type": "text",
			"text": text,
		},
	}
	if messageID != "" {
		update["messageId"] = messageID
	}
	return StreamEvent{Update: update}
}

func stringifyStreamValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case fmt.Stringer:
		return typed.String()
	default:
		raw, err := json.Marshal(typed)
		if err != nil {
			return fmt.Sprint(typed)
		}
		return string(raw)
	}
}
