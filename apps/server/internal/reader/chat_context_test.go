package reader

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChatKeepsLongSourceWithoutRepeatingIt(t *testing.T) {
	fakeAgent(t, "codex", "fail-before")
	s := testServer(t)
	if _, err := s.Store.DB.Exec("INSERT INTO documents(id,type,title,author,size,created_at,last_opened_at) VALUES('doc','pdf','test','',1,?,?)", now(), now()); err != nil {
		t.Fatal(err)
	}
	source := strings.Repeat("中文长资料。", 1800) + "独有原文事实：维修费用471元。"
	for _, m := range []Message{{Role: "user", Content: "费用？", Context: source}, {Role: "assistant", Content: "471元"}, {Role: "user", Content: "再次核对", Context: source}, {Role: "assistant", Content: "471元"}} {
		m.DocumentID = "doc"
		if err := s.Store.saveMessage(m); err != nil {
			t.Fatal(err)
		}
	}
	captured := ""
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		captured = req.Messages[1].Content
		writeAPIReply(w, "471元")
	}))
	defer api.Close()
	c := AIConfig{Primary: "codex", Models: map[string]string{}, TextAPI: APIConnection{URL: api.URL, Model: "test"}, Capabilities: map[string]Capability{}}
	c.Capabilities["text-api"] = Capability{Text: true, Fingerprint: configPrint(c, "text-api")}
	if err := s.writeAIConfig(c); err != nil {
		t.Fatal(err)
	}
	r := request(t, s, "POST", "/api/documents/doc/chat", strings.NewReader(`{"provider":"codex","prompt":"根据原始依据确认费用","context":""}`))
	if !strings.Contains(r.Body.String(), "event: done") {
		t.Fatal(r.Body.String())
	}
	if n := strings.Count(captured, "独有原文事实：维修费用471元。"); n != 1 {
		t.Errorf("original source fact occurs %d times, want once", n)
	}
	if strings.Contains(captured, "�") {
		t.Error("history damaged UTF-8")
	}
	if !strings.Contains(captured, "再次核对") {
		t.Error("source budget dropped the user question")
	}
}

func TestChatHistoryKeepsQuestionAnswerBoundaries(t *testing.T) {
	source := strings.Repeat("完整中文证据。", 6000)
	messages := []Message{{Role: "assistant", Content: "不应出现的孤立回答"}, {Role: "user", Content: "需要保留的问题", Context: source}, {Role: "assistant", Content: strings.Repeat("很长的中文回答。", 6000)}}
	prompt, chat := makeChatInput("doc", "title", "", "本轮问题", source, "", messages)
	if strings.Contains(prompt, "不应出现的孤立回答") {
		t.Fatal("included an orphan answer")
	}
	if !strings.Contains(prompt, "需要保留的问题") || !strings.Contains(prompt, "历史回答已截短") {
		t.Fatal("missing paired question or truncation marker")
	}
	if len(chat.Sources) != 1 || chat.Sources[0].Text != source {
		t.Fatal("source was truncated or duplicated")
	}
	if strings.Contains(prompt, "�") {
		t.Fatal("damaged UTF-8")
	}
}

func TestChatHistorySourcesStayWithinTotalBudget(t *testing.T) {
	old := strings.Repeat("旧", 150000/3)
	recent := strings.Repeat("近", 150000/3)
	current := strings.Repeat("今", 150000/3)
	messages := []Message{
		{Role: "user", Content: "第一问", Context: old}, {Role: "assistant", Content: "答一"},
		{Role: "user", Content: "第二问", Context: recent}, {Role: "assistant", Content: "答二"},
	}
	prompt, chat := makeChatInput("doc", "title", "", "第三问", current, "", messages)
	total := 0
	for _, source := range chat.Sources {
		total += len(source.Text)
		if source.Text == old {
			t.Fatal("kept the oldest source past the budget")
		}
	}
	if total > chatSourceBytes {
		t.Fatalf("sources total %d bytes", total)
	}
	if len(chat.Sources) != 2 || chat.Sources[1].Text != current {
		t.Fatal("dropped the current or most recent source")
	}
	if !strings.Contains(prompt, `"text":"第一问","sourceOmitted":true`) {
		t.Fatal("omitted source is not marked")
	}
}

func TestChatAddsDocumentSummaryWithoutImplicitExcerpt(t *testing.T) {
	s := testServer(t)
	d := organizationDoc(t, s)
	r := request(t, s, "PATCH", "/api/documents/"+d.ID, strings.NewReader(`{"metadata":{"abstract":"唯一摘要：讨论模型评估。"}}`))
	if r.Code != 200 {
		t.Fatal(r.Body)
	}
	upstream, calls := fakeAPI(t, func(int, apiCall) string { return "通用回答" })
	c := AIConfig{Primary: "api", API: APIConnection{URL: upstream.URL + "/v1", Key: "sk-test"}, Models: map[string]string{"api": "plain"}}
	passModels(&c, map[string]bool{"plain": false})
	if err := s.writeAIConfig(c); err != nil {
		t.Fatal(err)
	}
	r = request(t, s, "POST", "/api/documents/"+d.ID+"/chat", strings.NewReader(`{"provider":"api","prompt":"介绍一下Go的接口","context":""}`))
	if !strings.Contains(r.Body.String(), "event: done") {
		t.Fatal(r.Body)
	}
	prompt := calls()[0].Messages[1].Content.(string)
	if !strings.Contains(prompt, "唯一摘要：讨论模型评估。") || !strings.Contains(prompt, "介绍一下Go的接口") {
		t.Fatal(prompt)
	}
	if !strings.Contains(prompt, "Current source: \n") {
		t.Fatal("summary was treated as a current excerpt", prompt)
	}
	if strings.Contains(calls()[0].Messages[0].Content.(string), "Answer only from supplied content") {
		t.Fatal("general questions remain forbidden")
	}
	messages, err := s.Store.chatHistory(d.ID)
	if err != nil || len(messages) != 2 {
		t.Fatalf("%+v %v", messages, err)
	}
	if messages[0].Context != "" || len(messages[0].References) != 0 {
		t.Fatalf("invented a quotation: %+v", messages[0])
	}
}

func TestChatSummaryIsSeparateFromExplicitSources(t *testing.T) {
	source := "手动引用的完整原文"
	prompt, chat := makeChatInput("doc", "文章", "已有摘要", "继续解释", source, "", []Message{{Role: "user", Content: "上一问", Context: source}})
	if strings.Count(prompt, source) != 1 || len(chat.Sources) != 2 || chat.CurrentSource == chat.SummarySource {
		t.Fatalf("sources not deduplicated/separated: %s", prompt)
	}
	next := chat.incremental(map[string]bool{chat.SummarySource: true})
	if strings.Contains(next, "已有摘要") || !strings.Contains(next, "Document summary: "+chat.SummarySource) {
		t.Fatal(next)
	}
	_, empty := makeChatInput("doc", "文章", "", "一般问题", "", "", nil)
	if len(empty.Sources) != 0 || empty.CurrentSource != "" || empty.SummarySource != "" {
		t.Fatalf("invented sources: %+v", empty)
	}
}

func TestChatChapterPreferencePersists(t *testing.T) {
	s := testServer(t)
	if s.aiConfig().ChatIncludeChapter != nil {
		t.Fatal("chapter context should default off")
	}
	for _, enabled := range []bool{true, false} {
		c := AIConfig{ChatIncludeChapter: &enabled}
		if r := request(t, s, "PUT", "/api/ai/config", configJSON(c)); r.Code != 200 {
			t.Fatal(r.Body)
		}
		if r := request(t, s, "PUT", "/api/ai/config", strings.NewReader(`{"models":{}}`)); r.Code != 200 {
			t.Fatal(r.Body)
		}
		var got AIConfig
		r := request(t, s, "GET", "/api/ai/config", nil)
		if err := json.Unmarshal(r.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.ChatIncludeChapter == nil || *got.ChatIncludeChapter != enabled {
			t.Fatalf("preference lost after omitted update: %+v", got.ChatIncludeChapter)
		}
	}
}

func TestEveryChatKindIncludesTitleAndSummary(t *testing.T) {
	for _, kind := range []string{"ordinary", "selection", "image"} {
		t.Run(kind, func(t *testing.T) {
			s, _ := imageFixture(t)
			r := request(t, s, "PATCH", "/api/documents/doc", strings.NewReader(`{"title":"本次文章标题","metadata":{"abstract":"本次文章摘要"}}`))
			if r.Code != 200 {
				t.Fatal(r.Body)
			}
			upstream, calls := fakeAPI(t, func(int, apiCall) string { return "回答" })
			c := AIConfig{Primary: "api", API: APIConnection{URL: upstream.URL + "/v1", Key: "sk-test"}, Models: map[string]string{"api": "plain"}, VisionModels: map[string]string{"api": "plain"}}
			passModels(&c, map[string]bool{"plain": true})
			if err := s.writeAIConfig(c); err != nil {
				t.Fatal(err)
			}
			input := map[string]any{"provider": "api", "prompt": "问题", "context": ""}
			if kind == "selection" {
				input["context"] = "选区及完整辅助章节"
			}
			if kind == "image" {
				input["attachments"] = []string{"p1-b1"}
			}
			r = request(t, s, "POST", "/api/documents/doc/chat", strings.NewReader(jsonText(input)))
			if !strings.Contains(r.Body.String(), "event: done") {
				t.Fatal(r.Body)
			}
			payload := jsonText(calls()[0].Messages[1].Content)
			if !strings.Contains(payload, "本次文章标题") || !strings.Contains(payload, "本次文章摘要") {
				t.Fatal(payload)
			}
		})
	}
}

func TestChatAcceptsConfirmedLongContextUpToHardCap(t *testing.T) {
	s := testServer(t)
	d := organizationDoc(t, s)
	upstream, calls := fakeAPI(t, func(int, apiCall) string { return "回答" })
	c := AIConfig{Primary: "api", API: APIConnection{URL: upstream.URL + "/v1", Key: "sk-test"}, Models: map[string]string{"api": "plain"}}
	passModels(&c, map[string]bool{"plain": false})
	if err := s.writeAIConfig(c); err != nil {
		t.Fatal(err)
	}
	body := func(context string) *strings.Reader {
		b, _ := json.Marshal(map[string]string{"provider": "api", "prompt": "总结", "context": context})
		return strings.NewReader(string(b))
	}
	// About 100 KB, above the old 64 KB limit.
	long := strings.Repeat("证", 100000/3)
	r := request(t, s, "POST", "/api/documents/"+d.ID+"/chat", body(long))
	if !strings.Contains(r.Body.String(), "event: done") {
		t.Fatal(r.Body)
	}
	if !strings.Contains(jsonText(calls()[0].Messages[1].Content), long) {
		t.Fatal("long context was not sent whole")
	}
	r = request(t, s, "POST", "/api/documents/"+d.ID+"/chat", body(strings.Repeat("证", chatContextBytes/3+1)))
	if r.Code != 400 {
		t.Fatal(r.Code, r.Body)
	}
}
