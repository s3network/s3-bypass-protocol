package s3

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"
)

// webhookClientTimeout bounds the notification-configuration call.
//
// It is generous because the operation is not hot: one call per inbound, at
// startup. It exists at all because this call runs synchronously during inbound
// init, before the listener starts, so an object store that accepts the
// connection and then stops responding would otherwise leave the inbound down
// permanently with nothing logged.
const webhookClientTimeout = 30 * time.Second

// webhookHTTPClientInstance is the client used for the notification call.
//
// Not http.DefaultClient: that one has no timeout, it is process-global, and
// borrowing it means anything else in the program can change our timeout or
// inherit ours. ResponseHeaderTimeout is set as well as the overall deadline,
// because the failure this guards against is precisely a server that completes
// the handshake and then goes quiet — which an overall deadline covers, but one
// that reads as a timeout rather than as a mystery.
//
// It is built ONCE. The previous version constructed a whole
// Transport per call and never closed idle connections, so every SetupWebhook
// left its own private connection pool behind; the call happens once per inbound
// at startup, which is exactly the shape of a leak nobody notices until a
// config reload loop makes it one.
var webhookHTTPClientInstance = &http.Client{
	Timeout: webhookClientTimeout,
	Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ResponseHeaderTimeout: webhookClientTimeout,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
	},
}

// webhookHTTPClient returns the shared notification client.
func webhookHTTPClient() *http.Client { return webhookHTTPClientInstance }

// notificationConfig is the VK Cloud S3 SimpleTopicConfiguration document.
//
// It is built with encoding/xml rather than a format string. The prefix and the
// webhook URL both come from configuration, and a format string pastes them
// into the document raw: a prefix containing "<" closes the element early and
// the rest of the string is read as markup, so the configuration that gets
// installed is not the configuration that was asked for.
type notificationConfig struct {
	XMLName xml.Name `xml:"NotificationConfiguration"`
	Xmlns   string   `xml:"xmlns,attr"`
	Topic   simpleTopic
}

type simpleTopic struct {
	XMLName xml.Name `xml:"SimpleTopicConfiguration"`
	ID      string   `xml:"Id"`
	Event   string   `xml:"Event"`
	URL     string   `xml:"Url"`
	Filter  topicFilter
}

type topicFilter struct {
	S3Key s3KeyFilter
}

type s3KeyFilter struct {
	FilterRule []filterRule `xml:"FilterRule"`
}

type filterRule struct {
	Name  string `xml:"Name"`
	Value string `xml:"Value"`
}

func buildNotificationConfig(webhookURL, prefix string) ([]byte, error) {
	doc := notificationConfig{
		Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/",
		Topic: simpleTopic{
			ID:    "fedarisha",
			Event: "s3:ObjectCreated:Put",
			URL:   webhookURL,
			Filter: topicFilter{S3Key: s3KeyFilter{FilterRule: []filterRule{
				{Name: "Prefix", Value: prefix},
			}}},
		},
	}
	body, err := xml.MarshalIndent(doc, "", "   ")
	if err != nil {
		return nil, fmt.Errorf("build notification configuration: %w", err)
	}
	return append([]byte(xml.Header), body...), nil
}

// notificationURL builds the virtual-hosted bucket URL for the notification
// call: {scheme}://{bucket}.{host}/{path}?notification
//
// Parsed rather than sliced with TrimPrefix: an endpoint with a path, a port and
// no scheme, or a trailing slash produced a URL that pointed somewhere else
// entirely — and a mis-signed request to the wrong host fails as a signature
// error, which looks like bad credentials rather than a bad URL.
func notificationURL(endpoint, bucket, region string) (string, error) {
	if endpoint == "" {
		endpoint = fmt.Sprintf("https://s3.%s.amazonaws.com", region)
	}
	if !strings.Contains(endpoint, "://") {
		endpoint = "https://" + endpoint
	}
	base, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("parse storage endpoint %q: %w", endpoint, err)
	}
	if base.Host == "" {
		return "", fmt.Errorf("storage endpoint %q has no host", endpoint)
	}

	out := url.URL{
		Scheme:   base.Scheme,
		Host:     bucket + "." + base.Host,
		Path:     strings.TrimSuffix(base.Path, "/") + "/",
		RawQuery: "notification",
	}
	return out.String(), nil
}

// SetupWebhook configures S3 bucket notification to send ObjectCreated events
// to the given webhook URL. This uses the VK Cloud S3 SimpleTopicConfiguration
// extension (not standard AWS SNS/SQS).
//
// prefix filters events to keys starting with the given prefix (e.g. "projects/sessions/").
// webhookURL is the public HTTP endpoint that will receive POST notifications.
func (s *S3Store) SetupWebhook(ctx context.Context, webhookURL, prefix string) error {
	body, err := buildNotificationConfig(webhookURL, prefix)
	if err != nil {
		return err
	}

	target, err := notificationURL(s.cfg.Endpoint, s.cfg.Bucket, s.cfg.Region)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, target, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/xml")

	// Sign the request with AWS Signature V4.
	// VK Cloud S3 requires x-amz-content-sha256 as an explicit header.
	hash := sha256.Sum256(body)
	payloadHash := hex.EncodeToString(hash[:])
	req.Header.Set("x-amz-content-sha256", payloadHash)

	creds, err := credentials.NewStaticCredentialsProvider(
		s.cfg.AccessKey, s.cfg.SecretKey, "",
	).Retrieve(ctx)
	if err != nil {
		return fmt.Errorf("retrieve credentials: %w", err)
	}

	signer := v4.NewSigner()
	if err := signer.SignHTTP(ctx, creds, req, payloadHash, "s3", s.cfg.Region, time.Now()); err != nil {
		return fmt.Errorf("sign request: %w", err)
	}

	resp, err := webhookHTTPClient().Do(req)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return fmt.Errorf("S3 notification config failed (HTTP %d): %s", resp.StatusCode, string(respBody))
	}

	return nil
}
