package channel

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type bodyLimitTrackedReader struct {
	reads, closes int
}

func (b *bodyLimitTrackedReader) Read([]byte) (int, error) {
	b.reads++
	return 0, io.EOF
}

func (b *bodyLimitTrackedReader) Close() error {
	b.closes++
	return nil
}

func TestDoRequestCopilotBodyLimit(t *testing.T) {
	service.InitHttpClient()
	gin.SetMode(gin.TestMode)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(w, r.Body)
	}))
	defer server.Close()

	for _, tc := range []struct {
		name, origin, limit string
		size                int
		unknown, stream     bool
		wantStatus          int
	}{
		{name: "below", origin: server.URL, limit: "16", size: 15},
		{name: "exact", origin: server.URL, limit: "16", size: 16},
		{name: "over", origin: server.URL, limit: "16", size: 17, wantStatus: 413},
		{name: "stream_over_before_sse", origin: server.URL, limit: "16", size: 17, stream: true, wantStatus: 413},
		{name: "disabled", size: 17},
		{name: "different_origin", origin: "http://other.invalid", limit: "16", size: 17},
		{name: "not_prefix_match", origin: server.URL + "/path", limit: "16", size: 17},
		{name: "primary_in_list", origin: server.URL + ",http://secondary.invalid:9877", limit: "16", size: 17, wantStatus: 413},
		{name: "secondary_in_list", origin: "http://primary.invalid:9876, " + server.URL + " ,", limit: "16", size: 17, wantStatus: 413},
		{name: "list_unrelated", origin: "http://primary.invalid:9876,http://secondary.invalid:9877", limit: "16", size: 17},
		{name: "list_invalid_entries", origin: ",://invalid," + server.URL + "/path," + server.URL + "0", limit: "16", size: 17},
		{name: "unknown_length_not_measured", origin: server.URL, limit: "16", size: 17, unknown: true},
		{name: "invalid", origin: server.URL, limit: "SECRET_INVALID_CONFIG", size: 1, wantStatus: 500},
		{name: "zero", origin: server.URL, limit: "0", size: 1, wantStatus: 500},
		{name: "missing", origin: server.URL, size: 1, wantStatus: 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("COPILOT_PROXY_BODY_LIMIT_ORIGIN", tc.origin)
			t.Setenv("COPILOT_PROXY_MAX_REQUEST_BODY_BYTES", tc.limit)
			calls.Store(0)
			payload := strings.Repeat("x", tc.size)
			req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/responses?secret=QUERY_SECRET", strings.NewReader(payload))
			require.NoError(t, err)
			if tc.unknown {
				req.ContentLength = -1
			}
			var tracked *bodyLimitTrackedReader
			if tc.wantStatus != 0 {
				tracked = &bodyLimitTrackedReader{}
				req.Body = tracked
			}
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest(http.MethodPost, "/relay", nil)
			info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}, IsStream: tc.stream}
			resp, err := doRequest(ctx, req, info)
			if tc.wantStatus != 0 {
				require.Error(t, err)
				assert.Nil(t, resp)
				// Match the actual adaptor/Responses error wrapping chain.
				apiErr := types.NewOpenAIError(fmt.Errorf("do request failed: %w", err), types.ErrorCodeDoRequestFailed, 500)
				assert.Equal(t, tc.wantStatus, apiErr.StatusCode)
				assert.True(t, types.IsSkipRetryError(apiErr))
				assert.Zero(t, calls.Load())
				assert.Zero(t, tracked.reads)
				assert.Equal(t, 1, tracked.closes)
				assert.False(t, ctx.Writer.Written())
				assert.Empty(t, ctx.Writer.Header().Get("Content-Type"))
				assert.NotContains(t, err.Error(), "QUERY_SECRET")
				assert.NotContains(t, err.Error(), "SECRET_INVALID_CONFIG")
				if tc.wantStatus == 413 {
					assert.Contains(t, err.Error(), "17 bytes")
					assert.Contains(t, err.Error(), "16-byte limit")
				}
				return
			}
			require.NoError(t, err)
			defer resp.Body.Close()
			got, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			assert.True(t, bytes.Equal([]byte(payload), got))
			assert.EqualValues(t, 1, calls.Load())
			if req.GetBody != nil {
				replay, err := req.GetBody()
				require.NoError(t, err)
				defer replay.Close()
				raw, err := io.ReadAll(replay)
				require.NoError(t, err)
				assert.Equal(t, payload, string(raw))
			}
		})
	}
}

func TestDoRequestCopilotBodyLimitReplayableMetadata(t *testing.T) {
	t.Setenv("COPILOT_PROXY_BODY_LIMIT_ORIGIN", "http://copilot.invalid")
	t.Setenv("COPILOT_PROXY_MAX_REQUEST_BODY_BYTES", "16")
	payload := []byte("12345678901234567")
	body, storage := newPassThroughBody(t, payload)
	defer storage.Close()
	req, err := http.NewRequest(http.MethodPost, "http://copilot.invalid/v1/responses", body)
	require.NoError(t, err)
	assert.Zero(t, req.ContentLength, "net/http cannot infer the replayable body's size")
	ApplyUpstreamBodyMetadata(req, body)
	assert.EqualValues(t, len(payload), req.ContentLength)
	require.NotNil(t, req.GetBody)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/relay", nil)
	resp, err := doRequest(ctx, req, &relaycommon.RelayInfo{})
	require.Error(t, err)
	assert.Nil(t, resp)
	var apiErr *types.NewAPIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, http.StatusRequestEntityTooLarge, apiErr.StatusCode)
	// Local rejection closes only the request wrapper, not shared replay storage.
	replay, err := req.GetBody()
	require.NoError(t, err)
	defer replay.Close()
	got, err := io.ReadAll(replay)
	require.NoError(t, err)
	assert.Equal(t, payload, got)
	untouched, err := io.ReadAll(body)
	require.NoError(t, err)
	assert.Equal(t, payload, untouched, "preflight must not consume the body")
}
