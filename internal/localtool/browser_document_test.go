package localtool

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCloakBrowserSetDocument(t *testing.T) {
	m := installedCloakBrowser(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<title>original</title>"))
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	html := `<title>document fixture</title><script>window.documentWrites=(window.documentWrites||0)+1</script><button id="done" onclick="this.textContent='clicked'">go</button>`
	code := `page.goto(args['url'])
original = page.url
native = page.set_content.__func__
observed = set_document(args['html'], ready_selector='#done', timeout=5000)
page.locator('#done').click()
assert page.url == original
assert page.title() == 'document fixture'
assert page.locator('#done').inner_text() == 'clicked'
assert page.set_content.__func__ is native
assert page.evaluate('window.documentWrites') == 1
result = {'url':page.url,'wait':observed,'clicked':True}`
	cases := []struct {
		name, code string
		ok         bool
	}{
		{"document URL DOM click", code, true},
		{"explicit predicate timeout", `page.goto(args['url']); set_document(args['html'], ready_selector='#never-exists', timeout=100)`, false},
		{"timeout preserves one write", `page.goto(args['url'])
try:
    set_document(args['html'], ready_selector='#never-exists', timeout=100)
except Exception:
    assert page.evaluate('window.documentWrites') == 1
else:
    raise AssertionError('missing selector must fail')
result = {'writes':page.evaluate('window.documentWrites')}`, true},
		{"invalid selector error", `page.goto(args['url']); set_document(args['html'], ready_selector='[', timeout=1000)`, false},
		{"URL mutation error", `page.goto(args['url']); set_document('<script>history.replaceState(null,"","/changed")</script>', timeout=1000)`, false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			in, _ := json.Marshal(map[string]any{"code": test.code, "args": map[string]string{"url": server.URL, "html": html}})
			output, err := m.Execute(ctx, "document-fixture", "document", in)
			var response map[string]any
			if json.Unmarshal([]byte(output.Stdout), &response) != nil || (err == nil) != test.ok || response["ok"] != test.ok || response["cleanup"] != "completed" {
				t.Fatalf("document contract: %+v %v %s", response, err, output.Stderr)
			}
		})
	}
}
