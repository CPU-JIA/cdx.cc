package sse

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
)

type Event struct {
	Name string
	Data []byte
}

func Read(r io.Reader) (<-chan Event, <-chan error) {
	events := make(chan Event)
	errs := make(chan error, 1)

	go func() {
		defer close(events)
		defer close(errs)

		scanner := bufio.NewScanner(r)
		buf := make([]byte, 0, 64*1024)
		scanner.Buffer(buf, 2*1024*1024)

		var (
			name string
			data bytes.Buffer
		)

		flush := func() {
			if data.Len() == 0 && name == "" {
				return
			}
			raw := bytes.TrimSuffix(data.Bytes(), []byte("\n"))
			// 必须拷贝：data.Reset() 后 raw 指向的内存会被后续写入覆盖，
			// 而消费方协程可能还在对 payload 做 json.Unmarshal。
			payload := make([]byte, len(raw))
			copy(payload, raw)
			events <- Event{Name: name, Data: payload}
			name = ""
			data.Reset()
		}

		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				flush()
				continue
			}
			if strings.HasPrefix(line, "event:") {
				// 如果已有待发送的数据，先 flush 前一个事件
				// （防止上游没发空行分隔时数据拼接）
				if data.Len() > 0 {
					flush()
				}
				name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
				continue
			}
			if strings.HasPrefix(line, "data:") {
				data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
				data.WriteString("\n")
				continue
			}
			// SSE 注释行 (:) 和其他未知行静默忽略
		}
		if err := scanner.Err(); err != nil {
			errs <- err
			return
		}
		if name != "" || data.Len() > 0 {
			flush()
		}
	}()

	return events, errs
}

type Writer struct {
	w  io.Writer
	fl func()
}

func NewWriter(w io.Writer, flusher func()) *Writer {
	return &Writer{w: w, fl: flusher}
}

func (s *Writer) Send(name string, data []byte) error {
	if name == "" {
		return errors.New("missing event name")
	}
	if _, err := s.w.Write([]byte("event: " + name + "\n")); err != nil {
		return err
	}
	if len(data) > 0 {
		if _, err := s.w.Write([]byte("data: ")); err != nil {
			return err
		}
		if _, err := s.w.Write(data); err != nil {
			return err
		}
		if _, err := s.w.Write([]byte("\n")); err != nil {
			return err
		}
	}
	if _, err := s.w.Write([]byte("\n")); err != nil {
		return err
	}
	if s.fl != nil {
		s.fl()
	}
	return nil
}
