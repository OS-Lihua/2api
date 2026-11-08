package controller

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"sync"

	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/gin-gonic/gin"
)

// gatewayDeliveryWriter keeps pre-business framing private until the serving
// Model is known. An abandoned Attempt cannot commit headers for its successor.
// Content is held only in request memory and bounded before delivery.
type gatewayDeliveryWriter struct {
	gin.ResponseWriter
	mu        sync.Mutex
	pending   bytes.Buffer
	committed bool
	status    int
	err       error
}

func (w *gatewayDeliveryWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return 0, w.err
	}
	if w.committed {
		return w.ResponseWriter.Write(data)
	}
	if w.pending.Len()+len(data) > 1<<20 {
		w.err = errors.New("gateway response framing exceeds delivery buffer")
		return 0, w.err
	}
	return w.pending.Write(data)
}

func (w *gatewayDeliveryWriter) WriteString(data string) (int, error) {
	return w.Write([]byte(data))
}

func (w *gatewayDeliveryWriter) WriteHeader(status int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.committed {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	if status > 0 {
		w.status = status
	}
}

func (w *gatewayDeliveryWriter) WriteHeaderNow() {}

func (w *gatewayDeliveryWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.committed {
		w.ResponseWriter.Flush()
	}
}

func (w *gatewayDeliveryWriter) FlushError() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil || !w.committed {
		return w.err
	}
	var writer http.ResponseWriter = w.ResponseWriter
	for {
		if flusher, ok := writer.(interface{ FlushError() error }); ok {
			return flusher.FlushError()
		}
		unwrapper, ok := writer.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return nil
		}
		writer = unwrapper.Unwrap()
	}
}

// commit is called after the protocol parser has produced a complete business
// frame. A short write is never promoted into a billable delivery fact.
func (w *gatewayDeliveryWriter) commit(c *gin.Context) error {
	w.mu.Lock()
	if w.committed || w.err != nil {
		err := w.err
		w.mu.Unlock()
		return err
	}
	w.committed = true
	if w.status > 0 {
		w.ResponseWriter.WriteHeader(w.status)
	}
	data := w.pending.Bytes()
	n, err := w.ResponseWriter.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	w.pending.Reset()
	w.err = err
	w.mu.Unlock()
	if err != nil {
		return err
	}
	if err := helper.FlushWriter(c); err != nil {
		return &helper.StreamFlushError{Err: err}
	}
	return nil
}
