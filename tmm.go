// Package tmm provides a simple interface to the 10MinuteMail web service.
//   // Create a new session
//   s, err := tmm.New()
//   if err != nil {
// 	   log.Fatal(err)
//   }
//
//   // Check the email address
//   addr := s.Address()
//
//   // Retrieve all messages
//   mail, err := s.Messages()
//   for _, m := range mail {
// 	   fmt.Println(mail.Plaintext)
//   }
package tmm

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"time"

	tls "github.com/refraction-networking/utls"
	"github.com/zhangliwen/tmm/internal"
)

const (
	DefaultTimeout   = 10 * time.Second
	DateLayout       = "2006-01-02T15:04:05.000+00:00"
	DefaultUserAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/97.0.4692.99 Safari/537.36"

	baseURL = "https://10minutemail.com"

	endpointAddress     = "session/address"
	endpointExpired     = "session/expired"
	endpointReset       = "session/reset"
	endpointSecondsLeft = "session/secondsLeft"

	endpointMessagesAfter  = "messages/messagesAfter"
	endpointMessageReply   = "messages/reply"
	endpointMessageForward = "messages/forward"
)

var (
	ErrBuildingRequest = errors.New("failed to construct request object")
	ErrRequestFailed   = errors.New("request to 10minutemail failed")
	ErrReadBody        = errors.New("reading response body failed")
	ErrMarshalFailed   = errors.New("marshalling request body failed")
	ErrUnmarshalFailed = errors.New("unmarshalling response body failed")
	ErrMissingSession  = errors.New("missing session cookie in response")
	ErrBlockedByServer = errors.New("server is blocking requests from this host; probably rate limited")
)

// TLS fingerprint for Cloudflare bypass
var spec = &tls.ClientHelloSpec{
	CipherSuites: []uint16{
		49195,
		49196,
		52393,
		49199,
		49200,
		52392,
		158,
		159,
		49161,
		49162,
		49171,
		49172,
		51,
		57,
		156,
		157,
		47,
		53,
	},
	Extensions: []tls.TLSExtension{
		&tls.RenegotiationInfoExtension{
			Renegotiation: 0,
		},
		&tls.SNIExtension{
			ServerName: "",
		},
		&tls.UtlsExtendedMasterSecretExtension{},
		&tls.GenericExtension{
			Id:   35,
			Data: nil,
		},
		&tls.SignatureAlgorithmsExtension{
			SupportedSignatureAlgorithms: []tls.SignatureScheme{
				1027,
				1025,
			},
		},
		&tls.ALPNExtension{
			AlpnProtocols: []string{
				"http/1.1",
			},
		},
		&tls.SupportedPointsExtension{
			SupportedPoints: []uint8{
				0,
			},
		},
		&tls.SupportedCurvesExtension{
			Curves: []tls.CurveID{
				23,
			},
		},
	},
	TLSVersMin: 769,
	TLSVersMax: 771,
}

// Message represents a single email message sent to a temporary mail.
type Message struct {
	// The unique ID of the message on 10MinuteMail.
	// Not permanent.
	ID string `json:"id"`
	// The time the message was sent at.
	SentDate time.Time `json:"sentDate"`
	// The email address of the sender of the message.
	Sender string `json:"sender"`
	// The subject of the email.
	Subject string `json:"subject"`
	// The email body as received in plaintext, with HTML stripped.
	Plaintext string `json:"plaintext"`
	// The email body with HTML tags included.
	HTML string `json:"html"`
	// A short preview of the message body.
	Preview string `json:"preview"`
}

func (m *Message) UnmarshalJSON(data []byte) error {
	// Hacky workaround for custom time format.
	// See https://github.com/golang/go/issues/21990.
	type aux struct {
		ID        string `json:"id"`
		SentDate  string `json:"sentDate"`
		Sender    string `json:"sender"`
		Subject   string `json:"subject"`
		Plaintext string `json:"bodyPlainText"`
		HTML      string `json:"bodyHtmlContent"`
		Preview   string `json:"bodyPreview"`
	}

	v := &aux{}
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}

	m.ID = v.ID
	m.Sender = v.Sender
	m.Subject = v.Subject
	m.Plaintext = v.Plaintext
	m.HTML = v.HTML
	m.Preview = v.Preview

	// Custom time handler
	t, err := time.Parse(DateLayout, v.SentDate)
	if err != nil {
		return err
	}

	m.SentDate = t

	return nil
}

// Session holds information required to maintain a 10MinuteMail session.
type Session struct {
	address string
	token   string

	// The last time the session was reset.
	lastreset time.Time

	// The number of the last message fetched,
	// to ensure we aren't refetching the same data.
	lastcount int64

	baseurl string
	c       *http.Client
}

// headers returns the default set of headers to be sent with every request.
func (s *Session) headers() http.Header {
	return http.Header{
		"User-Agent": []string{DefaultUserAgent},
	}
}

// New creates a new 10MinuteMail session with a random address.
func New() (*Session, error) {
	s := &Session{
		baseurl: baseURL,
		c: &http.Client{
			Timeout: DefaultTimeout,
			Transport: &http.Transport{
				DialTLS: func(network, addr string) (net.Conn, error) {
					conn, err := net.Dial(network, addr)
					if err != nil {
						return nil, err
					}

					host, _, err := net.SplitHostPort(addr)
					if err != nil {
						return nil, err
					}

					config := &tls.Config{ServerName: host}
					uconn := tls.UClient(conn, config, tls.HelloCustom)
					if err := uconn.ApplyPreset(spec); err != nil {
						return nil, err
					}
					if err := uconn.Handshake(); err != nil {
						return nil, err
					}

					return uconn, nil
				},
			},
		},
		// It's better to assume that we have less time than more time.
		// Assume our mail will expire 10 minutes from initialisation,
		// before the request is made.
		lastreset: time.Now(),
	}

	return newSession(s)
}

// NewWithClient is identical to New but allows
// for passing a custom HTTP client object.
func NewWithClient(c *http.Client) (*Session, error) {
	s := &Session{
		baseurl:   baseURL,
		c:         c,
		lastreset: time.Now(),
	}

	return newSession(s)
}

// newSession abstracts the logic of the New function
// to enable testing.
func newSession(s *Session) (*Session, error) {
	u := join(baseURL, endpointAddress)
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return s, fmt.Errorf("%w: %s", ErrBuildingRequest, err)
	}

	req.Header = s.headers()

	// Initialise session
	res, err := s.c.Do(req)
	if err != nil {
		return s, fmt.Errorf("%w: %s", ErrRequestFailed, err)
	}
	defer res.Body.Close()

	if res.StatusCode == http.StatusForbidden {
		return s, ErrBlockedByServer
	}

	// Read body
	b, err := io.ReadAll(res.Body)
	if err != nil {
		return s, fmt.Errorf("%w: %s", ErrReadBody, err)
	}

	// Store session cookie
	for _, cookie := range res.Cookies() {
		if cookie.Name == "JSESSIONID" {
			s.token = cookie.Value
		}
	}
	if s.token == "" {
		return s, ErrMissingSession
	}

	// Store address
	v := &internal.AddressResponse{}
	err = json.Unmarshal(b, v)
	if err != nil {
		return s, fmt.Errorf("%w: %s", ErrUnmarshalFailed, err)
	}
	s.address = v.Address

	return s, nil
}

// Address returns the email address attached to the current session.
func (s *Session) Address() string {
	return s.address
}

// Expired returns whether or not the session is due to have expired
// and is in need of renewal.
func (s *Session) Expired() bool {
	return !time.Now().Before(s.lastreset.Add(10 * time.Minute))
}

// ExpiresAt returns a time.Time object representing the instant
// in time that the session is due to expire.
func (s *Session) ExpiresAt() time.Time {
	return s.lastreset.Add(10 * time.Minute)
}

// Messages contacts the server and returns a list of all messages
// received to the email address attached to this session.
//
// Note that if any new messages are found, the same counter will
// be updated that is used when calling the session.Latest() method,
// so you won't need to call it afterwards.
func (s *Session) Messages() ([]Message, error) {
	return s.messages(0)
}

// Latest contacts the server and returns a list of any messages
// that haven't already been received by this session.
func (s *Session) Latest() ([]Message, error) {
	return s.messages(s.lastcount)
}

func (s *Session) messages(i int64) ([]Message, error) {
	var m []Message

	// Prepare request
	u := join(s.baseurl, endpointMessagesAfter, strconv.FormatInt(i, 10))
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return m, fmt.Errorf("%w: %s", ErrBuildingRequest, err)
	}

	req.Header = s.headers()

	// Attach token
	req.AddCookie(&http.Cookie{
		Name:   "JSESSIONID",
		Value:  s.token,
		MaxAge: 300,
	})

	// Make request
	res, err := s.c.Do(req)
	if err != nil {
		return m, fmt.Errorf("%w: %s", ErrRequestFailed, err)
	}
	defer res.Body.Close()

	if res.StatusCode == http.StatusForbidden {
		return m, ErrBlockedByServer
	}

	// Read body
	b, err := io.ReadAll(res.Body)
	if err != nil {
		return m, fmt.Errorf("%w: %s", ErrReadBody, err)
	}

	// Unmarshal response
	err = json.Unmarshal(b, &m)
	if err != nil {
		return m, fmt.Errorf("%w: %s", ErrUnmarshalFailed, err)
	}

	// Update last received counter
	s.lastcount = i + int64(len(m))

	return m, nil
}

// Renew attempts to extend the session by an additional 10 minutes.
//
// Returns a bool indicating whether the server indicated that the
// reset was successful or not and an error if issues were encountered
// while making the request.
func (s *Session) Renew() (bool, error) {
	// If our reset was successful, assume that we have
	// 10 minutes from when this routine began, to be safe.
	resetAt := time.Now()

	// Prepare request
	u := join(s.baseurl, endpointReset)
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return false, fmt.Errorf("%w: %s", ErrBuildingRequest, err)
	}

	req.Header = s.headers()

	// Attach token
	req.AddCookie(&http.Cookie{
		Name:   "JSESSIONID",
		Value:  s.token,
		MaxAge: 300,
	})

	// Make request
	res, err := s.c.Do(req)
	if err != nil {
		return false, fmt.Errorf("%w: %s", ErrRequestFailed, err)
	}
	defer res.Body.Close()

	if res.StatusCode == http.StatusForbidden {
		return false, ErrBlockedByServer
	}

	// Read body
	b, err := io.ReadAll(res.Body)
	if err != nil {
		return false, fmt.Errorf("%w: %s", ErrReadBody, err)
	}

	// Unmarshal response
	v := &internal.ResetResponse{}
	err = json.Unmarshal(b, v)
	if err != nil {
		return false, fmt.Errorf("%w: %s", ErrUnmarshalFailed, err)
	}

	// As far as I know, this string indicates success
	if v.Response != "reset" {
		return false, nil
	}

	// Update reset time
	s.lastreset = resetAt

	return true, nil
}

// Reply asks 10MinuteMail to send a reply to the email that sent
// the message with the provided ID, with the provided body.
//
// Returns a bool indicating whether or not the reply was issued
// successfully - failure generally means the message is too old -
// and an error if issues were encountered while making the request.
func (s *Session) Reply(messageid, body string) (bool, error) {
	// Prepare body
	reqbody := &internal.ReplyRequest{}
	reqbody.Reply.MessageID = messageid
	reqbody.Reply.ReplyBody = body

	reqbytes, err := json.Marshal(reqbody)
	if err != nil {
		return false, fmt.Errorf("%w: %s", ErrMarshalFailed, err)
	}

	// Prepare request
	u := join(s.baseurl, endpointMessageReply)
	req, err := http.NewRequest(http.MethodPost, u, bytes.NewReader(reqbytes))
	if err != nil {
		return false, fmt.Errorf("%w: %s", ErrBuildingRequest, err)
	}

	req.Header = s.headers()

	// Attach token
	req.AddCookie(&http.Cookie{
		Name:   "JSESSIONID",
		Value:  s.token,
		MaxAge: 300,
	})

	// Make request
	res, err := s.c.Do(req)
	if err != nil {
		return false, fmt.Errorf("%w: %s", ErrRequestFailed, err)
	}
	defer res.Body.Close()

	// Check status code to determine result
	switch res.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusForbidden:
		return false, ErrBlockedByServer
	default:
		return false, nil
	}
}

// Forward asks 10MinuteMail to forward the message with the
// provided ID to the provided recipient address.
//
// Returns a bool indicating whether or not the forward request was
// issued successfully and an error if issues were encountered while
// making the request.
//
// Note that the server will claim to be successful even if the recipient
// address is invalid or the mail gets rejected after sending.
func (s *Session) Forward(messageid, recipient string) (bool, error) {
	// Prepare body
	reqbody := &internal.ForwardRequest{}
	reqbody.Forward.MessageID = messageid
	reqbody.Forward.ForwardAddress = recipient

	reqbytes, err := json.Marshal(reqbody)
	if err != nil {
		return false, fmt.Errorf("%w: %s", ErrMarshalFailed, err)
	}

	// Prepare request
	u := join(s.baseurl, endpointMessageForward)
	req, err := http.NewRequest(http.MethodPost, u, bytes.NewReader(reqbytes))
	if err != nil {
		return false, fmt.Errorf("%w: %s", ErrBuildingRequest, err)
	}

	req.Header = s.headers()

	// Set headers
	req.Header.Add("Content-Type", "application/json")

	// Attach token
	req.AddCookie(&http.Cookie{
		Name:   "JSESSIONID",
		Value:  s.token,
		MaxAge: 300,
	})

	// Make request
	res, err := s.c.Do(req)
	if err != nil {
		return false, fmt.Errorf("%w: %s", ErrRequestFailed, err)
	}
	defer res.Body.Close()

	// Check status code to determine result
	switch res.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusForbidden:
		return false, ErrBlockedByServer
	default:
		return false, nil
	}
}

// join concatinates URL components.
func join(b string, n ...string) string {
	u, err := url.Parse(b)
	if err != nil {
		// should never happen..
		panic(err)
	}
	for _, str := range n {
		u.Path = path.Join(u.Path, str)
	}

	return u.String()
}
