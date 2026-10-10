package plugin

import (
	"bytes"
	"encoding/json"
	"net/url"
	"testing"
)

func TestExtractHTML(t *testing.T) {
	page := "<!doctype html><svg></svg>"
	for answer, want := range map[string]string{
		"说明\n```css\nbody {}\n```\n```HTML\n" + page + "\n```": page,
		"```\n" + page + "\n```":                               page,
		"  " + page + "\n":                                     page,
		"```js\nlet svg = 1\n```\n" + page:                     "",
		"下面是页面：" + page:                                        "",
	} {
		if got := extractHTML(answer); got != want {
			t.Errorf("extractHTML(%q) = %q, want %q", answer, got, want)
		}
	}
}

func TestPelicanRun(t *testing.T) {
	setupTest(t)
	answers := map[string]json.RawMessage{
		"page": mockModelResponse("好的：\n```html\n<svg></svg>\n```"),
		"tool": json.RawMessage(`{"status":"completed","output":[{"type":"custom_tool_call"}],"usage":{}}`),
	}
	hostCall = streamHost(func(method string, payload any) (json.RawMessage, error) {
		if method == "host.auth.list" {
			return json.RawMessage(`{"files":[{"id":"page","provider":"codex"},{"id":"tool","provider":"codex"}]}`), nil
		}
		// "none" leaves the effort to CPA.
		if body, prompt := sentTurn(payload); prompt != pelicanPrompt || body["reasoning"].(map[string]any)["effort"] != nil {
			t.Errorf("prompt = %q, reasoning = %v", prompt, body["reasoning"])
		}
		return answers[payload.(map[string]any)["auth_id"].(string)], nil
	})
	if response := pelicanRunResponse([]byte(`{"all":true,"model":"m","effort":"none"}`)); response.StatusCode != 200 {
		t.Fatalf("run: %s", response.Body)
	}
	tasks.Wait()
	page, tool := pelicanResults["page"][0], pelicanResults["tool"][0]
	if page.HTML != "<svg></svg>" || page.Error != "" || page.OutputTokens != 3 || tool.HTML != "" || tool.Error != "模型调用了工具，没有直接回答" {
		t.Fatalf("page = %+v, tool = %+v", page, tool)
	}
	// Lists carry summaries; the page loads with its record.
	if state := stateResponse(); bytes.Contains(state.Body, []byte("svg")) || !bytes.Contains(state.Body, []byte(page.ID)) {
		t.Fatalf("state = %s", state.Body)
	}
	var record pelicanResult
	if err := json.Unmarshal(recordResponse("pelican", url.Values{"auth_id": {"page"}, "id": {page.ID}}).Body, &record); err != nil || record.HTML != page.HTML {
		t.Fatalf("record = %+v, %v", record, err)
	}
	pelicanResults, loaded = map[string][]pelicanResult{}, false
	loadState()
	if len(pelicanResults["page"]) != 1 || pelicanResults["page"][0].HTML != page.HTML {
		t.Fatal("pelican history did not survive reload")
	}
}

// A panic during a background run must end that run, not the host process.
func TestBackgroundRunsSurvivePanics(t *testing.T) {
	setupTest(t)
	hostCall = func(method string, _ any) (json.RawMessage, error) {
		if method == "host.auth.list" {
			return json.RawMessage(`{"files":[{"id":"a","provider":"codex"}]}`), nil
		}
		panic("boom")
	}
	if response := candyRunResponse([]byte(`{"all":true,"model":"m","effort":"low","runs":2}`)); response.StatusCode != 200 {
		t.Fatalf("candy run: %s", response.Body)
	}
	tasks.Wait() // A credential runs one test at a time.
	if response := pelicanRunResponse([]byte(`{"all":true,"model":"m","effort":"low"}`)); response.StatusCode != 200 {
		t.Fatalf("pelican run: %s", response.Body)
	}
	tasks.Wait()
	mu.Lock()
	defer mu.Unlock()
	if len(candyResults["a"]) != 2 || candyResults["a"][0].Error == "" || candyRunning["a"] != nil {
		t.Fatalf("candy = %+v, running = %v", candyResults["a"], candyRunning["a"])
	}
	if len(pelicanResults["a"]) != 1 || pelicanResults["a"][0].Error == "" || pelicanRunning["a"] {
		t.Fatalf("pelican = %+v, running = %v", pelicanResults["a"], pelicanRunning["a"])
	}
}
