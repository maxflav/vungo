package openrtbutil

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Vungle/openrtb"
	"golang.org/x/net/context"
)

// MakeSimpleRequest sends a dummy bid request to the specified url just to get some bid response
// object; the url endpoint should generally not validate the bid request object for authenticity.
func MakeSimpleRequest(t *testing.T, url string) (*Response, error) {
	ctx := context.Background()
	br := &openrtb.BidRequest{
		Id: "没%创%意%真%可%怕",
	}

	req, err := NewRequest(ctx, br, url)
	if err != nil {
		t.Fatal("Cannot create a bid request: ", err)
	}

	c := NewClient(nil)
	return c.Do(req)
}

func TestClientDoShouldDiscardResidualAndReuseConnection(t *testing.T) {
	// Given a test server that always leaves some residual payload at the end.
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
		resp.Header().Set("Content-Type", "application/json")
		resp.Write([]byte(`{"id":"abc","seatbid":[{"bid":[{"id":"1"}]}]}\nahz`))
	}))

	var connCounter uint32 = 0
	ts.Config.ConnState = func(conn net.Conn, connState http.ConnState) {
		if connState == http.StateNew {
			atomic.AddUint32(&connCounter, 1)
		}
	}

	ts.Start()
	defer ts.Close()

	idle := make(chan struct{}, 1)
	connDiscardHook = func(*http.Response) {
		idle <- struct{}{}
	}
	defer func() {
		connDiscardHook = nil
	}()

	// Make a simple request.
	if _, err := MakeSimpleRequest(t, ts.URL); err != nil {
		t.Error("Bid request should not respond with error: ", err)
	}

	<-idle

	// Make another simple request after payload has been discarded.
	if _, err := MakeSimpleRequest(t, ts.URL); err != nil {
		t.Error("Bid request should not respond with error: ", err)
	}

	if connCounter > 1 {
		t.Error("No more than one connection should have been created.")
	}
}

func TestClientDoShouldDiscardResidualOnInvalidHttpResponse(t *testing.T) {
	tests := []struct {
		header  string
		status  int
		payload []byte
	}{
		// When response set the wrong Content-Type header.
		{"text/json", http.StatusOK, []byte(`{"id":"abc","seatbid":[{"bid":[{}]}]}`)},

		// When response set HTTP status to be 204 No Content, but still has body payload.
		{"application/json", http.StatusNoContent, []byte(`{"id":"abc","seatbid":[{"bid":[{"id":"1"}]}]}\nahz`)},

		// When response set HTTP status to be anything other than 200 OK status code.
		{"application/json", http.StatusBadRequest, []byte(`{"id":"abc","seatbid":[{"bid":[{}]}]}`)},
		{"application/json", http.StatusUnauthorized, []byte(`{"id":"abc","seatbid":[{"bid":[{}]}]}`)},
		{"application/json", http.StatusHTTPVersionNotSupported, []byte(`{"id":"abc","seatbid":[{"bid":[{}]}]}`)},

		// When response sent non-JSON body:
		{"application/json", http.StatusOK, []byte(`this is not even JSON`)},
	}

	// Given a test server that would return invalid HTTP responses for OpenRTB spec.
	var middleware http.HandlerFunc
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
		middleware(resp, req)
	}))

	var connCounter uint32 = 0
	ts.Config.ConnState = func(conn net.Conn, connState http.ConnState) {
		if connState == http.StateNew {
			atomic.AddUint32(&connCounter, 1)
		}
	}

	ts.Start()
	defer ts.Close()

	idle := make(chan struct{}, 1)
	connDiscardHook = func(*http.Response) {
		idle <- struct{}{}
	}
	defer func() {
		connDiscardHook = nil
	}()

	// When making requests one at a time.
	for i, test := range tests {
		t.Logf("Testing %d...", i)

		middleware = func(resp http.ResponseWriter, req *http.Request) {
			if len(test.header) > 0 {
				resp.Header().Set("Content-Type", test.header)
			}

			resp.WriteHeader(test.status)

			if len(test.payload) > 0 {
				resp.Write(test.payload)
			}
		}

		ctx := context.Background()
		br := &openrtb.BidRequest{
			Id: "br-for-no-bid",
		}

		req, err := NewRequest(ctx, br, ts.URL)
		if err != nil {
			t.Fatal("Cannot create a bid request: ", err)
		}

		_, err = DefaultClient.Do(req)
		switch err.(type) {
		case NoBidError:
		default:
			t.Error("Expected no bid instead of ", err)
		}

		<-idle
	}

	// Expect only one connection is used.
	if connCounter != 1 {
		t.Error("Underlying HTTP client should have made exactly one persistent connection instead of ", connCounter)
	}
}

func TestClientDoShouldGiveUpDiscardingOnSlowConnection(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	// Given a test server that takes forever to respond the body.
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
		resp.Header().Set("Content-Type", "application/json")
		resp.WriteHeader(200)
		// Respond and flush with a full valid JSON first then go to sleep.
		resp.Write([]byte(`{"id":"abc","seatbid":[{"bid":[{"id":"1"}]}]}`))
		resp.(http.Flusher).Flush()
		time.Sleep(5 * time.Second)
		resp.Write([]byte(`oh, I'm not done yet!`))
	}))

	var connCounter uint32 = 0
	ts.Config.ConnState = func(conn net.Conn, connState http.ConnState) {
		if connState == http.StateNew {
			atomic.AddUint32(&connCounter, 1)
		}
	}

	ts.Start()
	defer ts.Close()

	idle := make(chan struct{}, 1)
	connDiscardHook = func(*http.Response) {
		idle <- struct{}{}
	}
	defer func() {
		connDiscardHook = nil
	}()

	// When making a simple bid request.
	if resp, err := MakeSimpleRequest(t, ts.URL); err != nil {
		t.Error("There should be a valid bid.", err)
	} else if resp == nil || resp.OpenRtb().Id != "abc" {
		t.Error("Unexpected resp: ", resp)
	}

	select {
	case <-idle:
	case <-time.After(defaultDiscardDuration * 2):
		t.Error("Test took took long!")
	}

	// And then make another simple bid request.
	if resp, err := MakeSimpleRequest(t, ts.URL); err != nil {
		t.Error("There should be a valid bid.", err)
	} else if resp == nil || resp.OpenRtb().Id != "abc" {
		t.Error("Unexpected resp: ", resp)
	}

	// Expects that the new connections to have been made rather than reusing the same connection.
	if connCounter != 2 {
		t.Errorf("Client should have established 2 TCP connection instead of %d because the discard on one connection took too long!", connCounter)
	}
}

func TestClientDoHttp(t *testing.T) {
	ctype := "omg/json"

	ts := httptest.NewServer(http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
		resp.Header().Set("Content-Type", ctype)
		resp.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()

	body := bytes.NewBufferString(`{}`)
	req, err := http.NewRequest(http.MethodPost, ts.URL, body)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := DefaultClient.doHttp(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}

	if resp.StatusCode != http.StatusNoContent {
		t.Error("Response should complete with no content.")
	}

	if ct := resp.Header.Get("Content-Type"); ct != ctype {
		t.Errorf("Content-Type header should be %s instead of %s.", ctype, ct)
	}
}

func TestClientDoHttpShouldDiscardResidualPayloadWhenContextCompletes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	// Given a test server where the governing context is done while HTTP response is being written.
	ts := httptest.NewServer(http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
		resp.Write([]byte("hi"))
		cancel()
		resp.Write([]byte("ther"))
	}))
	defer ts.Close()

	body := bytes.NewBufferString(`{}`)
	req, err := http.NewRequest(http.MethodPost, ts.URL, body)
	if err != nil {
		t.Fatal(err)
	}

	idle := make(chan struct{})
	connDiscardHook = func(*http.Response) {
		close(idle)
	}
	defer func() {
		connDiscardHook = nil
	}()

	// When making an HTTP request.
	_, err = DefaultClient.doHttp(ctx, req)
	if err != context.Canceled {
		t.Fatal(err)
	}

	// Expect connection to have been discarded.
	select {
	case <-idle:
	case <-time.After(time.Second):
		t.Error("Test took too long!")
	}
}

func TestClientHandleNoBid(t *testing.T) {
	tests := []struct {
		status  int
		headers http.Header
		body    io.Reader
		nbr     openrtb.NoBidReason
		logged  string
	}{
		{http.StatusNoContent, http.Header{"Content-Type": []string{"application/json"}}, bytes.NewReader([]byte("extra payload")), openrtb.NO_BID_HTTP_NO_CONTENT, "extra payload"},
		{http.StatusBadGateway, http.Header{"Content-Type": []string{"application/json"}}, bytes.NewReader([]byte("extra payload")), openrtb.NO_BID_NON_STANDARD_HTTP_STATUS, "extra payload"},
		{http.StatusOK, http.Header{"Content-Type": []string{"some-non/json"}}, bytes.NewReader([]byte("{}")), openrtb.NO_BID_INVALID_HTTP_HEADER, ""},
	}

	logged := bytes.NewBuffer(nil)
	l := log.New(logged, "", 0)
	c := NewClient(l)
	for i, test := range tests {
		t.Logf("Testing %d...", i)
		logged.Reset()

		url, err := url.Parse("http://localhost")
		if err != nil {
			t.Fatal(err)
		}
		resp := &http.Response{
			Request:    &http.Request{URL: url},
			StatusCode: test.status,
			Header:     test.headers,
			Body:       ioutil.NopCloser(test.body),
		}

		if err, ok := c.handleNoBid(resp).(NoBidError); !ok || err == nil {
			t.Error("handleNoBid should return a NoBidError.")
		} else if err.Reason() != test.nbr {
			t.Errorf("Expected no bid with reason %v instead of %v.", test.nbr, err.Reason())
		}

		if len(test.logged) > 0 && logged.String() != fmt.Sprintf("client received unnecessary body: http://localhost, '%s...'\n", test.logged) {
			t.Errorf("Expected logger to have logged\n%s instead of\n%s.", test.logged, logged.String())
		}
	}
}