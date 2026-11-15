package httpapi

import (
	"context"
	"io"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/Schallophy/literouter/internal/model"
	"github.com/Schallophy/literouter/internal/util"
)

type pipeOptions struct {
	IdleTimeout time.Duration
	Context     context.Context
	Headers     map[string]string
	OnData      func(string)
}

type pipeResult struct {
	Usage *model.Usage
	Bytes int64
	Error *string
}

// pipeSSE forwards the provider stream byte-for-byte while tracking usage.
func pipeSSE(ctx *Ctx, response *http.Response, options pipeOptions) pipeResult {
	header := ctx.W.Header()
	header.Set("content-type", "text/event-stream; charset=utf-8")
	header.Set("cache-control", "no-cache, no-transform")
	header.Set("connection", "keep-alive")
	header.Set("x-accel-buffering", "no")
	for key, value := range options.Headers {
		header.Set(key, value)
	}
	ctx.W.WriteHeader(200)
	ctx.W.Flush()

	requestContext := options.Context
	if requestContext == nil {
		requestContext = context.Background()
	}
	idle := options.IdleTimeout
	if idle <= 0 {
		idle = 120 * time.Second
	}

	tracker := &util.SSEUsageTracker{}
	decoder := &utf8Stream{}
	var bytes int64
	var failure *string

	closeBody := func() { _ = response.Body.Close() }
	timer := time.AfterFunc(idle, closeBody)
	defer timer.Stop()
	stopContext := context.AfterFunc(requestContext, closeBody)
	defer stopContext()

	buffer := make([]byte, 32*1024)
	for {
		read, err := response.Body.Read(buffer)
		if read > 0 {
			bytes += int64(read)
			timer.Reset(idle)
			text := decoder.Decode(buffer[:read])
			if text != "" {
				tracker.Push(text)
				if options.OnData != nil {
					options.OnData(text)
				}
			}
			if _, writeErr := ctx.W.Write(buffer[:read]); writeErr != nil {
				message := writeErr.Error()
				failure = &message
				break
			}
			ctx.W.Flush()
		}
		if err != nil {
			if err != io.EOF {
				message := err.Error()
				failure = &message
			}
			break
		}
	}
	return pipeResult{Usage: tracker.Usage(), Bytes: bytes, Error: failure}
}

// utf8Stream keeps an incomplete trailing rune between reads.
type utf8Stream struct {
	pending []byte
}

func (d *utf8Stream) Decode(chunk []byte) string {
	if len(d.pending) > 0 {
		chunk = append(append([]byte(nil), d.pending...), chunk...)
		d.pending = nil
	}
	if len(chunk) == 0 {
		return ""
	}
	for index := len(chunk) - 1; index >= 0 && index >= len(chunk)-3; index-- {
		if chunk[index] < utf8.RuneSelf {
			break
		}
		if utf8.RuneStart(chunk[index]) {
			if !utf8.FullRune(chunk[index:]) {
				d.pending = append([]byte(nil), chunk[index:]...)
				chunk = chunk[:index]
			}
			break
		}
	}
	return string(chunk)
}
