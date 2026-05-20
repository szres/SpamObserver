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

	prompt := fmt.Sprintf(`你是一个Telegram群组反垃圾/反诈骗分析助手。请根据以下用户信息判断其风险等级。

重要：风险等级的后果不同——"确认spam"会自动封禁用户，"高风险"会提交人工审核。因此"确认spam"必须是确凿无疑的垃圾/诈骗号，而可疑但不确定的应判为"高风险"。

用户信息：
- Name: %s
- Username: @%s
- Bio: %s

═══ 判断标准 ═══

【确认spam】— 必须确凿无疑，符合以下任意一条：

1. 金融推广/诈骗：Name或Bio中包含明确的虚拟货币/金融推广词汇，如：虚拟货币搬砖、日挣/日入+金额、币安上押、USDT搬砖、搞米、搞钱、赚钱暗语
2. 赚钱诱导+链接：Bio中包含明确的赚钱诱导内容+外部链接(如t.me/+邀请链接)，如：一天保你XXX、带几个缺钱的兄弟、只要你肯付出
3. 明确金融违规：Name或Bio中明确包含转账、代付、洗钱、跑分、卡农等金融违规词汇
4. 批量注册特征：Name本身是一个金钱/金融相关词汇(如：辅导费、代付款、转账、佣金、回扣、返现、红包、结算) + Username为空 + Bio为空——三个条件同时满足，典型的批量注册垃圾号

【高风险】— 疑似spam，需人工审核确认，符合以下任意一条：

1. 诈骗地域关联：Name中包含东南亚/中东诈骗高发地区名或相关词汇，如：妙瓦底、缅北、缅甸、柬埔寨、西港、金边、菲律宾、马尼拉、迪拜、老挝、KK园区
2. 金钱词汇作Name：Name含金钱/金融相关词汇(费/钱/币/款/元/万/收益/利润/佣金/回扣/工资) + Username为空或Bio为空（但未同时满足"确认spam-4"的三条件）
3. 随机Username+可疑元素：Username为随机字母数字组合(如mwd11101、abc123、user888、x4y2zz) + Name或Bio中存在任何可疑元素(地域、金融、防御性声明等)
4. 防御性声明：Bio为"此地无银三百两"式自我辩护，包含"遵纪守法"、"只做合法"、"支持国家"、"法律允许"、"合法合规"等描述，尤其配合可疑Name或随机Username——真实守法用户不会特意声明
5. 信息不一致：Name、Username、Bio三者信息严重不一致(如Name暗示某身份但Username为随机串且Bio为空)
6. 空Profile+可疑Name：Username和Bio均为空 + Name为非常见人名或含可疑词汇

【中风险】— 部分可疑但证据不足：

1. Username为随机组合但Bio为空，Name无可疑
2. 有可疑关键词但上下文不明确
3. Bio为空 + Name为普通词汇但非常见人名

【低风险】— 正常用户：Name为常见人名或正常昵称，Username有意义，Bio正常或为空

═══ 分析要点 ═══

- 重点关注Name、Username、Bio三者之间的关联性和一致性
- 多个弱信号叠加应升级风险等级(如：可疑Name + 随机Username + 空Bio → 高风险)
- 中文Name中的金钱相关词汇是强信号，即使看似"正常"(如"辅导费"本身是一个词，但作为人名极不正常)
- 诈骗地名出现在Name中(如"妙瓦底的神")应判为高风险
- 随机字母数字混合Username(如mwd11101)是机器人/批量注册的典型特征

请仅返回以下JSON格式，不要有其他内容：
{"risk_level":"低/中/高/确认spam","reason":"简要说明判断原因，必须指出具体触发了哪条规则"}`, name, username, bio)

	reqBody := chatRequest{
		Model: cfg.Model,
		Messages: []chatMessage{
			{Role: "system", Content: "你是反垃圾反诈骗分析专家。只返回JSON格式结果，不要有其他内容。"},
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
