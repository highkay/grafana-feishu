package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/basicauth"
	"github.com/gofiber/fiber/v2/middleware/logger"
	openai "github.com/sashabaranov/go-openai"
)

const systemPrompt = `作为一名经验丰富的SRE运维专家，请分析以下告警信息。以精简、清晰的Markdown格式（兼容飞书卡片）返回分析结果，必须包含以下部分：

### 故障分析
- **告警摘要**: [一句话概括问题]
- **可能原因**: [列出1-3个最可能的原因]
- **影响范围**: [说明此问题可能造成的影响]

### 处置建议
- **排查步骤**:
  - [可直接执行的命令或检查步骤1]
  - [可直接执行的命令或检查步骤2]
- **恢复操作**:
  - [用于恢复服务的命令]
- **根本原因分析建议**:
  - [定位根本原因的建议或命令]

请确保所有命令都包裹在 Markdown 代码块中以便复制执行。请使用中文回复。`

type Notification struct {
	Receiver          string            `json:"receiver"`
	Status            string            `json:"status"`
	OrgID             int               `json:"orgId"`
	Alerts            []Alert           `json:"alerts"`
	GroupLabels       map[string]string `json:"groupLabels"`
	CommonLabels      map[string]string `json:"commonLabels"`
	CommonAnnotations map[string]string `json:"commonAnnotations"`
	ExternalURL       string            `json:"externalURL"`
	Version           string            `json:"version"`
	GroupKey          string            `json:"groupKey"`
	TruncatedAlerts   int               `json:"truncatedAlerts"`
	Title             string            `json:"title"`
	State             string            `json:"state"`
	Message           string            `json:"message"`
}

type Alert struct {
	Status       string            `json:"status"`
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     string            `json:"startsAt"`
	EndsAt       string            `json:"endsAt"`
	GeneratorURL string            `json:"generatorURL"`
	Fingerprint  string            `json:"fingerprint"`
	SilenceURL   string            `json:"silenceURL"`
	DashboardURL string            `json:"dashboardURL"`
	PanelURL     string            `json:"panelURL"`
	Values       map[string]interface{} `json:"values"`
}

type FeishuCard struct {
	MsgType string            `json:"msg_type"`
	Card    FeishuCardContent `json:"card"`
}

type FeishuCardContent struct {
	Header   FeishuCardHeader       `json:"header"`
	Elements []FeishuCardDivElement `json:"elements"`
}

type FeishuCardHeader struct {
	Title    FeishuCardTextElement `json:"title"`
	Template string                `json:"template"`
}

type FeishuCardTextElement struct {
	Tag     string `json:"tag"`
	Content string `json:"content"`
}

type FeishuCardDivElement struct {
	Tag  string                `json:"tag"`
	Text FeishuCardTextElement `json:"text"`
}

var defaultWebhookBase string = "https://open.feishu.cn/open-apis/bot/v2/hook"

func main() {
	var feishuWebhookBase string
	var defaultBotUUID string

	configuredWebhook := os.Getenv("FEISHU_WEBHOOK")
	if configuredWebhook != "" {
		re := regexp.MustCompile(`^(.*)/?([-a-z0-9]{36})?$`)
		matches := re.FindStringSubmatch(configuredWebhook)
		feishuWebhookBase = matches[1]
		defaultBotUUID = matches[2]
	} else {
		feishuWebhookBase = strings.TrimRight(os.Getenv("FEISHU_WEBHOOK_BASE"), "/")
		if feishuWebhookBase == "" {
			feishuWebhookBase = defaultWebhookBase
		}
		defaultBotUUID = os.Getenv("FEISHU_WEBHOOK_UUID")
	}
	if defaultBotUUID == "" {
		log.Println("defaultUUID not provided")
	}

	openaiToken := os.Getenv("OPENAI_API_KEY")
	openaiBaseURL := os.Getenv("OPENAI_BASE_URL")
	openaiModelName := os.Getenv("OPENAI_MODEL_NAME")
	if openaiModelName == "" {
		openaiModelName = openai.GPT3Dot5Turbo
	}

	app := fiber.New()
	app.Use(logger.New())

	webhookAuth := os.Getenv("WEBHOOK_AUTH")
	if webhookAuth != "" {
		log.Printf("Enabling basic auth")
		parts := strings.SplitN(webhookAuth, ":", 2)
		app.Use(basicauth.New(basicauth.Config{
			Users: map[string]string{
				parts[0]: parts[1],
			},
		}))
	}

	app.Post("/:botUUID?", func(c *fiber.Ctx) error {
		c.Accepts("application/json")
		notification := new(Notification)
		if err := c.BodyParser(notification); err != nil {
			return err
		}

		title, ok := notification.CommonAnnotations["summary"]
		if !ok {
			title = notification.Title
		}

		description, ok := notification.CommonAnnotations["description"]
		if !ok {
			description = notification.Message
		}

		color := "red"
		if notification.Status == "resolved" {
			color = "green"
		}

		if openaiToken != "" {
			log.Printf("Calling OpenAI API for more details...")
			config := openai.DefaultConfig(openaiToken)
			if openaiBaseURL != "" {
				config.BaseURL = openaiBaseURL
			}
			client := openai.NewClientWithConfig(config)
			resp, err := client.CreateChatCompletion(
				context.Background(),
				openai.ChatCompletionRequest{
					Model: openaiModelName,
					Messages: []openai.ChatCompletionMessage{
						{
							Role:    openai.ChatMessageRoleSystem,
							Content: systemPrompt,
						},
						{
							Role:    openai.ChatMessageRoleUser,
							Content: description,
						},
					},
				},
			)
			if err != nil {
				log.Printf("OpenAI API call failed: %v", err)
				description = "OpenAI API call failed: " + err.Error()
			} else {
				description = strings.Trim(resp.Choices[0].Message.Content, "```markdown\n")
				description = strings.Trim(description, "```")
				log.Printf("Description from OpenAI: %s", description)
			}
		}

		feishuCard := &FeishuCard{
			MsgType: "interactive",
			Card: FeishuCardContent{
				Header: FeishuCardHeader{
					Title: FeishuCardTextElement{
						Tag:     "plain_text",
						Content: title,
					},
					Template: color,
				},
				Elements: []FeishuCardDivElement{
					{
						Tag: "div",
						Text: FeishuCardTextElement{
							Tag:     "lark_md",
							Content: description,
						},
					},
				},
			},
		}
		feishuJson, err := json.Marshal(feishuCard)
		if err != nil {
			return err
		}
		log.Printf("Feishu card JSON: %s", string(feishuJson))
		botUUID := c.Params("botUUID", defaultBotUUID)
		feishuWebhookURL := feishuWebhookBase + "/" + botUUID
		request, err := http.NewRequest("POST", feishuWebhookURL, bytes.NewBuffer(feishuJson))
		request.Header.Set("Content-Type", "application/json; charset=UTF-8")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		body, _ := ioutil.ReadAll(response.Body)
		log.Printf("Response body: %s", string(body))

		return c.SendStatus(204)
	})

	app.Listen(":2387")
}