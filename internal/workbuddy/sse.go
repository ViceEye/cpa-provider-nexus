package workbuddy

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// SSE records may span arbitrary host HTTP chunks. Emit one JSON completion
// per plugin frame; CPA owns client framing and the terminal DONE marker.
type completionStream struct {
	pending   []byte
	data      []byte
	sawChoice bool
	finished  bool
	done      bool
}

const maxSSERecordBytes = 16 << 20

func (s *completionStream) Feed(raw []byte) ([][]byte, error) {
	s.pending = append(s.pending, raw...)
	var frames [][]byte
	for {
		end := bytes.IndexByte(s.pending, '\n')
		if end < 0 {
			break
		}
		if end > maxSSERecordBytes {
			return nil, fmt.Errorf("SSE line too large")
		}
		line := bytes.TrimSuffix(s.pending[:end], []byte("\r"))
		s.pending = s.pending[end+1:]
		if len(line) == 0 {
			frame, err := s.dispatch()
			if err != nil {
				return nil, err
			}
			if len(frame) > 0 {
				frames = append(frames, frame)
			}
		} else if bytes.HasPrefix(line, []byte("data:")) {
			value := bytes.TrimPrefix(line[5:], []byte(" "))
			if len(s.data)+len(value)+1 > maxSSERecordBytes {
				return nil, fmt.Errorf("SSE event too large")
			}
			if len(s.data) > 0 {
				s.data = append(s.data, '\n')
			}
			s.data = append(s.data, value...)
		}
	}
	if len(s.pending) > maxSSERecordBytes {
		return nil, fmt.Errorf("SSE line too large")
	}
	return frames, nil
}

func (s *completionStream) dispatch() ([]byte, error) {
	data := bytes.TrimSpace(s.data)
	s.data = nil
	if len(data) == 0 || s.done {
		return nil, nil
	}
	if bytes.Equal(data, []byte("[DONE]")) {
		s.done = true
		return nil, nil
	}
	var chunk struct {
		Error   json.RawMessage `json:"error"`
		Choices []struct {
			Index        int     `json:"index"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(data, &chunk); err != nil {
		return nil, fmt.Errorf("invalid completion SSE JSON")
	}
	if len(chunk.Error) > 0 && string(chunk.Error) != "null" {
		return nil, fmt.Errorf("WorkBuddy upstream returned an SSE error")
	}
	if len(chunk.Choices) == 0 && (len(chunk.Usage) == 0 || string(chunk.Usage) == "null") {
		return nil, fmt.Errorf("SSE event is not a completion")
	}
	for _, choice := range chunk.Choices {
		if choice.Index != 0 {
			return nil, fmt.Errorf("multiple choices are unsupported")
		}
		s.sawChoice = true
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			s.finished = true
		}
	}
	return bytes.Clone(data), nil
}

func (s *completionStream) Finish() ([][]byte, error) {
	// Accept a complete final data line without its optional trailing newline.
	frames, err := s.Feed([]byte("\n\n"))
	if err != nil {
		return nil, err
	}
	if !s.sawChoice || (!s.done && !s.finished) {
		return nil, fmt.Errorf("empty or truncated WorkBuddy completion stream")
	}
	return frames, nil
}
