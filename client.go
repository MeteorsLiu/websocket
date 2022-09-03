package websocket

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"net"
	"time"

	"github.com/valyala/fasthttp"
)

var (
	// ErrCannotUpgrade shows up when an error occurred when upgrading a connection.
	ErrCannotUpgrade = errors.New("cannot upgrade connection")
)

// MakeClient returns Conn using an existing connection.
//
// url must be a complete URL format i.e. http://localhost:8080/ws
func MakeClient(c net.Conn, url string) (*Client, error) {
	return client(c, url, nil)
}

// ClientWithHeaders returns a Conn using an existing connection and sending custom headers.
func ClientWithHeaders(c net.Conn, url string, req *fasthttp.Request) (*Client, error) {
	return client(c, url, req)
}

// UpgradeAsClient will upgrade the connection as a client
//
// This function should be used with connections that intend to use a
// plain framing.
//
// r can be nil.
func UpgradeAsClient(c net.Conn, url string, r *fasthttp.Request) error {
	req := fasthttp.AcquireRequest()
	res := fasthttp.AcquireResponse()
	uri := fasthttp.AcquireURI()

	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(res)
	defer fasthttp.ReleaseURI(uri)

	uri.Update(url)

	origin := bytePool.Get().([]byte)
	key := bytePool.Get().([]byte)
	defer bytePool.Put(origin)
	defer bytePool.Put(key)

	origin = prepareOrigin(origin, uri)
	key = makeRandKey(key[:0])

	if r != nil {
		r.CopyTo(req)
	}

	req.Header.SetMethod("GET")
	req.Header.AddBytesKV(originString, origin)
	req.Header.AddBytesKV(connectionString, upgradeString)
	req.Header.AddBytesKV(upgradeString, websocketString)
	req.Header.AddBytesKV(wsHeaderVersion, supportedVersions[0])
	req.Header.AddBytesKV(wsHeaderKey, key)
	// TODO: Add compression

	req.SetRequestURIBytes(uri.FullURI())

	br := bufio.NewReader(c)
	bw := bufio.NewWriter(c)
	req.Write(bw)
	bw.Flush()

	err := res.Read(br)
	if err == nil {
		if res.StatusCode() != 101 ||
			!equalsFold(res.Header.PeekBytes(upgradeString), websocketString) {
			err = ErrCannotUpgrade
		}
	}

	return err
}

func client(c net.Conn, url string, r *fasthttp.Request) (cl *Client, err error) {
	err = UpgradeAsClient(c, url, r)
	if err == nil {
		cl = &Client{
			c: c,
			brw: bufio.NewReadWriter(
				bufio.NewReader(c), bufio.NewWriter(c)),
		}
	}

	return cl, err
}

// Dial establishes a websocket connection as client.
//
// url parameter must follow the WebSocket URL format i.e. ws://host:port/path
func Dial(url string) (*Client, error) {
	cnf := &tls.Config{
		InsecureSkipVerify: false,
		MinVersion:         tls.VersionTLS11,
		MaxVersion:         tls.VersionTLS13,
	}

	return dial(url, cnf, nil)
}

// Dial with context
func DialContext(url string, dialer *net.Dialer) (*Client, error) {
	cnf := &tls.Config{
		InsecureSkipVerify: false,
		MinVersion:         tls.VersionTLS11,
		MaxVersion:         tls.VersionTLS13,
	}

	return dial(url, cnf, nil, dialer)
}

// DialTLS establishes a websocket connection as client with the
// tls.Config. The config will be used if the URL is wss:// like.
func DialTLS(url string, cnf *tls.Config) (*Client, error) {
	return dial(url, cnf, nil)
}

// DialWithHeaders establishes a websocket connection as client sending a personalized request.
func DialWithHeaders(url string, req *fasthttp.Request) (*Client, error) {
	cnf := &tls.Config{
		InsecureSkipVerify: false,
		MinVersion:         tls.VersionTLS11,
	}

	return dial(url, cnf, req)
}

func dial(url string, cnf *tls.Config, req *fasthttp.Request, dialers ...*net.Dialer) (conn *Client, err error) {
	uri := fasthttp.AcquireURI()
	defer fasthttp.ReleaseURI(uri)

	uri.Update(url)

	scheme := "https"
	port := ":443"
	if bytes.Equal(uri.Scheme(), wsString) {
		scheme, port = "http", ":80"
	}
	uri.SetScheme(scheme)

	addr := bytePool.Get().([]byte)
	defer bytePool.Put(addr)

	addr = append(addr[:0], uri.Host()...)
	if n := bytes.LastIndexByte(addr, ':'); n == -1 {
		addr = append(addr, port...)
	}

	var c net.Conn
	var dialer *net.Dialer
	if len(dialers) > 0 {
		dialer = dialers[0]
	} else {
		dialer = &net.Dialer{}
	}
	if scheme == "http" {
		c, err = dialer.Dial("tcp", b2s(addr))
	} else {
		c, err = tls.Dial("tcp", b2s(addr), cnf)
	}

	if err == nil {
		conn, err = client(c, uri.String(), req)
		if err != nil {
			c.Close()
		}
	}
	return conn, err
}

func makeRandKey(b []byte) []byte {
	b = extendByteSlice(b, 16)
	rand.Read(b[:16])
	b = appendEncode(base64, b[:0], b[:16])
	return b
}

// Client holds a WebSocket connection.
//
// The client is NOT concurrently safe. It is intended to be
// used with the Frame struct.
type Client struct {
	c   net.Conn
	brw *bufio.ReadWriter
}

// Write writes the content `b` as text.
//
// To send binary content use WriteBinary.
func (c *Client) Write(b []byte) (int, error) {
	fr := AcquireFrame()
	defer ReleaseFrame(fr)

	fr.SetFin()
	fr.SetPayload(b)
	fr.SetText()
	fr.Mask()

	return c.WriteFrame(fr)
}

// WriteBinary writes the content `b` as binary.
//
// To send text content use Write.
func (c *Client) WriteBinary(b []byte) (int, error) {
	fr := AcquireFrame()
	defer ReleaseFrame(fr)

	fr.SetFin()
	fr.SetPayload(b)
	fr.SetBinary()
	fr.Mask()

	return c.WriteFrame(fr)
}

// WriteFrame writes the frame into the WebSocket connection.
func (c *Client) WriteFrame(fr *Frame) (int, error) {
	nn, err := fr.WriteTo(c.brw)
	if err == nil {
		err = c.brw.Flush()
	}

	return int(nn), err
}

// ReadFrame reads a frame from the connection.
func (c *Client) ReadFrame(fr *Frame) (int, error) {
	n, err := fr.ReadFrom(c.brw)
	return int(n), err
}

// Close gracefully closes the websocket connection.
func (c *Client) Close() error {
	fr := AcquireFrame()
	fr.SetClose()
	fr.SetFin()

	fr.SetStatus(StatusNone)

	_, err := c.WriteFrame(fr)
	if err != nil {
		return err
	}

	c.c.SetReadDeadline(time.Now().Add(time.Second * 3)) // wait 3 seconds before closing
	// just read the next message
	c.ReadFrame(fr)

	return c.c.Close()
}
