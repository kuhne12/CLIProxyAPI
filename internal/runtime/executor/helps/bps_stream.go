package helps

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// BPSBody releases account admission only after the stream has closed.
type BPSBody struct {
	io.ReadCloser
	Release func()
	once    sync.Once
}

func (b *BPSBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.Release)
	return err
}

// ConsumeBPSEvents reads the normalized single-data-line SSE emitted by the adapter.
func ConsumeBPSEvents(body io.Reader, consume func([]byte) error) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		if !bytes.HasPrefix(line, []byte("data: ")) {
			continue
		}
		if err := consume(line[6:]); err != nil {
			return err
		}
	}
	if errScan := scanner.Err(); errScan != nil {
		var failure *pluginabi.Error
		if errors.As(errScan, &failure) || errors.Is(errScan, context.Canceled) || errors.Is(errScan, context.DeadlineExceeded) {
			return errScan
		}
		return BPSMissingTerminal()
	}
	return nil
}

func BPSMissingTerminal() error {
	return &pluginabi.Error{Code: pluginabi.ErrorCodeStreamTruncated, HTTPStatus: 502, Message: "BPS stream ended without a completed response; generation was not replayed"}
}
