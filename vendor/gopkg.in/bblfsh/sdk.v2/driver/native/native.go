package native

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

	"context"

	"gopkg.in/bblfsh/sdk.v2/driver"
	"gopkg.in/bblfsh/sdk.v2/driver/native/jsonlines"
	"gopkg.in/bblfsh/sdk.v2/uast/nodes"
	"gopkg.in/src-d/go-errors.v1"
)

var (
	// Binary default location of the native driver binary. Should not
	// override this variable unless you know what are you doing.
	Binary = "/opt/driver/bin/native"
)

var (
	ErrNotRunning = errors.NewKind("native driver is not running")
)

func NewDriver(enc Encoding) driver.Native {
	return NewDriverAt("", enc)
}

func NewDriverAt(bin string, enc Encoding) driver.Native {
	if bin == "" {
		bin = Binary
	}
	if enc == "" {
		enc = UTF8
	}
	return &Driver{bin: bin, ec: enc}
}

// Driver is a wrapper of the native command. The operations with the
// driver are synchronous by design, this is controlled by a mutex. This means
// that only one parse request can attend at the same time.
type Driver struct {
	bin     string
	ec      Encoding
	running bool

	mu     sync.Mutex
	enc    jsonlines.Encoder
	dec    jsonlines.Decoder
	stdin  io.Closer
	stdout io.Closer
	cmd    *exec.Cmd
}

// Start executes the given native driver and prepares it to parse code.
func (d *Driver) Start() error {
	d.cmd = exec.Command(d.bin)
	d.cmd.Stderr = os.Stderr

	stdin, err := d.cmd.StdinPipe()
	if err != nil {
		return err
	}

	stdout, err := d.cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return err
	}

	d.stdin = stdin
	d.stdout = stdout
	d.enc = jsonlines.NewEncoder(stdin)
	d.dec = jsonlines.NewDecoder(stdout)

	err = d.cmd.Start()
	if err == nil {
		d.running = true
		return nil
	}
	stdin.Close()
	stdout.Close()
	return err
}

// parseRequest is the request used to communicate the driver with the
// native driver via json.
type parseRequest struct {
	Content  string   `json:"content"`
	Encoding Encoding `json:"Encoding"`
}

var _ json.Unmarshaler = (*parseResponse)(nil)

// parseResponse is the reply to parseRequest by the native parser.
type parseResponse struct {
	Status status     `json:"status"`
	Errors []string   `json:"errors"`
	AST    nodes.Node `json:"ast"`
}

func (r *parseResponse) UnmarshalJSON(data []byte) error {
	var resp struct {
		Status status      `json:"status"`
		Errors []string    `json:"errors"`
		AST    interface{} `json:"ast"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return err
	}
	ast, err := nodes.ToNode(resp.AST, nil)
	if err != nil {
		return err
	}
	*r = parseResponse{
		Status: resp.Status,
		Errors: resp.Errors,
		AST:    ast,
	}
	return nil
}

// Parse sends a request to the native driver and returns its response.
func (d *Driver) Parse(ctx context.Context, src string) (nodes.Node, error) {
	if !d.running {
		return nil, ErrNotRunning.New()
	}

	str, err := d.ec.Encode(src)
	if err != nil {
		return nil, err
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	err = d.enc.Encode(&parseRequest{
		Content: str, Encoding: d.ec,
	})
	if err != nil {
		// Cannot write data - this means the stream is broken or driver crashed.
		// We will try to recover by reading the response, but since it might be
		// a stack trace or an error message, we will read it as a "raw" value.
		// This preserves an original text instead of failing with decoding error.
		var raw json.RawMessage
		// TODO: this reads a single line only; we can be smarter and read the whole log if driver cannot recover
		if err := d.dec.Decode(&raw); err != nil {
			// stream is broken on both sides, cannot get additional info
			return nil, err
		}
		return nil, fmt.Errorf("error: %v; %s", err, string(raw))
	}

	var r parseResponse
	if err := d.dec.Decode(&r); err != nil {
		return nil, err
	}
	switch r.Status {
	case statusOK:
		return r.AST, nil
	case statusError:
		return nil, driver.PartialParse(r.AST, r.Errors)
	case statusFatal:
		return nil, driver.MultiError(r.Errors)
	default:
		return nil, fmt.Errorf("unsupported status: %v", r.Status)
	}
}

// Stop stops the execution of the native driver.
func (d *Driver) Close() error {
	var last error
	if err := d.stdin.Close(); err != nil {
		last = err
	}
	err := d.cmd.Wait()
	err2 := d.stdout.Close()
	if err != nil {
		return err
	}
	if er, ok := err2.(*os.PathError); ok && er.Err == os.ErrClosed {
		err2 = nil
	}
	if err2 != nil {
		last = err2
	}
	return last
}

var _ json.Unmarshaler = (*status)(nil)

type status string

func (s *status) UnmarshalJSON(data []byte) error {
	var str string
	if err := json.Unmarshal(data, &str); err != nil {
		return err
	}
	str = strings.ToLower(str)
	*s = status(str)
	return nil
}

const (
	statusOK = status("ok")
	// statusError is replied when the driver has got the AST with errors.
	statusError = status("error")
	// statusFatal is replied when the driver hasn't could get the AST.
	statusFatal = status("fatal")
)

var _ json.Unmarshaler = (*Encoding)(nil)

// Encoding is the Encoding used for the content string. Currently only
// UTF-8 or Base64 encodings are supported. You should use UTF-8 if you can
// and Base64 as a fallback.
type Encoding string

const (
	UTF8   = Encoding("utf8")
	Base64 = Encoding("base64")
)

func (e *Encoding) UnmarshalJSON(data []byte) error {
	var str string
	if err := json.Unmarshal(data, &str); err != nil {
		return err
	}
	str = strings.ToLower(str)
	*e = Encoding(str)
	return nil
}

// Encode converts UTF8 string into specified Encoding.
func (e Encoding) Encode(s string) (string, error) {
	switch e {
	case UTF8:
		return s, nil
	case Base64:
		s = base64.StdEncoding.EncodeToString([]byte(s))
		return s, nil
	default:
		return "", fmt.Errorf("invalid Encoding: %v", e)
	}
}

// Decode converts specified Encoding into UTF8.
func (e Encoding) Decode(s string) (string, error) {
	switch e {
	case UTF8:
		return s, nil
	case Base64:
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return "", err
		}
		return string(b), nil
	default:
		return "", fmt.Errorf("invalid Encoding: %v", e)
	}
}
