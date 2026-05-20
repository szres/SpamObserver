package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Config struct {
	BaseURL string
	APIKey  string
	Model   string
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type AssessResult struct {
	RiskLevel string `json:"risk_level"`
	Reason    string `json:"reason"`
	Duration  time.Duration
}

func BuildAPIURL(baseURL string) string {
	u := strings.TrimSpace(baseURL)
	u = strings.TrimRight(u, "/")
	if strings.HasSuffix(u, "/v1/chat/completions") {
		return u
	}
	if strings.HasSuffix(u, "/v1") {
		return u + "/chat/completions"
	}
	if strings.HasSuffix(u, "/chat/completions") {
		return u
	}
	return u + "/v1/chat/completions"
}

func noRedirectClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func truncateBody(b []byte, max int) string {
	s := strings.TrimSpace(string(b))
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}

func AssessUser(ctx context.Context, cfg Config, name, username, bio string) (*AssessResult, error) {
	start := time.Now()

	prompt := fmt.Sprintf(`你是一个Telegram群组反垃圾/反广告分析助手。请根据以下用户信息判断其为广告、垃圾信息或诈骗用户的风险等级。

用户信息：
- Name: %s
- Username: @%s
- Bio: %s

判断标准：

【确认spam】— 以下任意一条即确认为spam：
1. Name或Bio中包含明显的虚拟货币/金融推广词汇，如：虚拟货币搬砖、日挣/日入+金额、币安上押、USDT搬砖、搞米、赚钱暗语
2. Bio中包含明显的赚钱诱导内容+外部链接(如t.me/+邀请链接)，如：一天保你XXX、带几个缺钱的兄弟、只要你肯付出
3. Name或Bio中包含转账、代付、洗钱等金融违规词汇+可疑Username

【高风险】— 以下特征组合：
1. Username和Bio均为空或无意义
2. Username为随机字母数字组合(如xxxla3、daxiaole1、z4y1wwd等) + Bio含可疑内容
3. Name使用常见中文昵称但配合可疑Bio
4. Bio含单一可疑词但无明显诱导行为

【中风险】— 部分可疑但不明显：
1. Username为随机组合但Bio为空
2. 有可疑关键词但上下文不明确

【低风险】— 正常用户

请仅返回以下JSON格式，不要有其他内容：
{"risk_level":"低/中/高/确认spam","reason":"简要说明判断原因"}`, name, username, bio)

	reqBody := chatRequest{
		Model: cfg.Model,
		Messages: []chatMessage{
			{Role: "system", Content: "你是一个反垃圾分析助手，只返回JSON格式结果。"},
			{Role: "user", Content: prompt},
		},
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	url := BuildAPIURL(cfg.BaseURL)
	client := noRedirectClient(30 * time.Second)

	var respBody []byte
	const maxRetries = 3
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(1 * time.Second)
		}

		httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(bodyBytes))
		if err != nil {
			return nil, fmt.Errorf("create request: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Authorization", "Bearer "+cfg.APIKey)

		resp, err := client.Do(httpReq)
		if err != nil {
			return nil, fmt.Errorf("http request: %w", err)
		}
		respBody, err = io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("read response: %w", err)
		}

		if resp.StatusCode == http.StatusTooManyRequests && attempt < maxRetries-1 {
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("request to %s returned status %d: %s", url, resp.StatusCode, truncateBody(respBody, 200))
		}
		break
	}

	var chatResp chatResponse
	if err := json.Unmarshal(respBody, &chatResp); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}

	if chatResp.Error != nil {
		return nil, fmt.Errorf("API error: %s", chatResp.Error.Message)
	}

	if len(chatResp.Choices) == 0 {
		return nil, fmt.Errorf("no choices in response")
	}

	duration := time.Since(start)

	content := chatResp.Choices[0].Message.Content
	content = strings.TrimSpace(content)

	var result AssessResult
	if err := json.Unmarshal([]byte(content), &result); err != nil {
		result = AssessResult{
			RiskLevel: "未知",
			Reason:    content,
		}
	}
	result.Duration = duration

	return &result, nil
}

func TestConnection(ctx context.Context, cfg Config) error {
	reqBody := chatRequest{
		Model: cfg.Model,
		Messages: []chatMessage{
			{Role: "user", Content: "Hello, reply with 'ok' only."},
		},
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	url := BuildAPIURL(cfg.BaseURL)
	client := noRedirectClient(15 * time.Second)

	const maxRetries = 3
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(1 * time.Second)
		}

		httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(bodyBytes))
		if err != nil {
			return fmt.Errorf("create request: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Authorization", "Bearer "+cfg.APIKey)

		resp, err := client.Do(httpReq)
		if err != nil {
			return fmt.Errorf("request to %s failed: %w", url, err)
		}
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode == http.StatusTooManyRequests && attempt < maxRetries-1 {
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("request to %s returned status %d: %s", url, resp.StatusCode, truncateBody(respBody, 200))
		}
		return nil
	}
	return nil
}
