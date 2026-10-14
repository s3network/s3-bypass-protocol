package s3

import (
	"encoding/xml"
	"strings"
	"testing"
)

// The document used to be a fmt.Sprintf with the prefix and the URL pasted in
// raw, so a prefix carrying "<" closed the element early and everything after
// it was installed as markup — the configuration that got applied was not the
// configuration that was asked for.
func TestNotificationConfigEscapesThePrefixAndURL(t *testing.T) {
	body, err := buildNotificationConfig("https://hooks.example/webhook?x=1&y=2", `evil"><DeleteBucket>&`)
	if err != nil {
		t.Fatalf("buildNotificationConfig: %v", err)
	}

	var doc struct {
		XMLName xml.Name
		Topic   struct {
			URL    string `xml:"Url"`
			Filter struct {
				S3Key struct {
					Rule []struct {
						Name  string `xml:"Name"`
						Value string `xml:"Value"`
					} `xml:"FilterRule"`
				} `xml:"S3Key"`
			} `xml:"Filter"`
		} `xml:"SimpleTopicConfiguration"`
	}
	if err := xml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("the document must be well-formed XML: %v\n%s", err, body)
	}
	if doc.XMLName.Local != "NotificationConfiguration" {
		t.Errorf("root element = %q, want NotificationConfiguration", doc.XMLName.Local)
	}
	if want := "https://hooks.example/webhook?x=1&y=2"; doc.Topic.URL != want {
		t.Errorf("Url = %q, want %q (the ampersands must survive as data)", doc.Topic.URL, want)
	}
	if len(doc.Topic.Filter.S3Key.Rule) != 1 {
		t.Fatalf("got %d filter rules, want 1", len(doc.Topic.Filter.S3Key.Rule))
	}
	if want := `evil"><DeleteBucket>&`; doc.Topic.Filter.S3Key.Rule[0].Value != want {
		t.Errorf("Prefix filter = %q, want %q", doc.Topic.Filter.S3Key.Rule[0].Value, want)
	}
	if strings.Contains(string(body), "<DeleteBucket>") {
		t.Errorf("the prefix leaked into the document as markup:\n%s", body)
	}
}

// The URL used to be assembled by trimming "https://" off the endpoint and
// pasting the remainder between two literals. An endpoint with a path or a
// trailing slash produced a URL pointing somewhere else, and a wrongly-signed
// request fails as a credentials error rather than as a URL error.
func TestNotificationURLUsesTheParsedEndpoint(t *testing.T) {
	cases := []struct {
		endpoint, bucket, region, want string
	}{
		{"https://s3.example.com", "b", "eu-central-1", "https://b.s3.example.com/?notification"},
		{"http://127.0.0.1:9000", "b", "us-east-1", "http://b.127.0.0.1:9000/?notification"},
		{"http://127.0.0.1:9000/", "b", "us-east-1", "http://b.127.0.0.1:9000/?notification"},
		{"127.0.0.1:9000", "b", "us-east-1", "https://b.127.0.0.1:9000/?notification"},
		{"", "b", "us-east-1", "https://b.s3.us-east-1.amazonaws.com/?notification"},
	}
	for _, c := range cases {
		got, err := notificationURL(c.endpoint, c.bucket, c.region)
		if err != nil {
			t.Errorf("endpoint %q: %v", c.endpoint, err)
			continue
		}
		if got != c.want {
			t.Errorf("endpoint %q → %q, want %q", c.endpoint, got, c.want)
		}
	}
}
