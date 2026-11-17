package im

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Tencent/WeKnora/internal/infrastructure/docparser"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
)

// imDefaultAttachmentParseWait bounds a rule-picked parser when neither the
// agent nor WEKNORA_CHAT_ATTACHMENT_WAIT_TIMEOUT_SEC sets a wait. It matches
// the web chat's default.
const imDefaultAttachmentParseWait = 60 * time.Second

// imAttachmentParserEngine picks the parser for an IM attachment the way the
// agent editor's chat attachment table (聊天附件解析策略) shows it: the agent's
// rule for the file type, then the workspace's chat rule, then the type's
// default engine, which is the one the table marks 默认. An empty result keeps
// IM's own routing (readIMAttachmentDefault).
func imAttachmentParserEngine(agent *types.CustomAgent, tenant *types.Tenant, fileType string) string {
	engine := ""
	if agent != nil {
		engine = chatParserEngineRule(agent.Config.ChatParserEngineRules, fileType)
	}
	if engine == "" && tenant != nil && tenant.ParserEngineConfig != nil {
		engine = chatParserEngineRule(tenant.ParserEngineConfig.ChatParserEngineRules, fileType)
	}
	if engine == "" {
		engine = strings.TrimSpace(types.DefaultParserEngine(fileType))
	}
	if strings.EqualFold(engine, "auto") {
		return ""
	}
	return engine
}

// chatParserEngineRule returns the engine of the first rule that names this
// file type, compared without case, spaces or a leading dot; "" when none does.
func chatParserEngineRule(rules []types.ParserEngineRule, fileType string) string {
	want := normalizeIMFileType(fileType)
	if want == "" {
		return ""
	}
	for _, rule := range rules {
		engine := strings.TrimSpace(rule.Engine)
		if engine == "" {
			continue
		}
		for _, candidate := range rule.FileTypes {
			if normalizeIMFileType(candidate) == want {
				return engine
			}
		}
	}
	return ""
}

func normalizeIMFileType(fileType string) string {
	return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(fileType)), ".")
}

// imEngineIsDefaultReader reports whether an engine is one of the two readers
// IM's own routing already uses (docreader's builtin parser and the Go reader
// for simple formats). Falling back from one of them would read the file the
// same way a second time.
func imEngineIsDefaultReader(engine string) bool {
	return engine == docparser.BuiltinEngineName || engine == docparser.SimpleEngineName
}

// imAttachmentParseWait bounds a rule-picked parser: the agent's
// 附件解析等待超时 (attachment_parse_wait_timeout_sec), then
// WEKNORA_CHAT_ATTACHMENT_WAIT_TIMEOUT_SEC, then 60 s, the same order the web
// chat uses for its attachments.
func imAttachmentParseWait(agent *types.CustomAgent) time.Duration {
	if agent != nil && agent.Config.AttachmentParseWaitTimeoutSec > 0 {
		return time.Duration(agent.Config.AttachmentParseWaitTimeoutSec) * time.Second
	}
	if raw := strings.TrimSpace(os.Getenv("WEKNORA_CHAT_ATTACHMENT_WAIT_TIMEOUT_SEC")); raw != "" {
		if sec, err := strconv.Atoi(raw); err == nil && sec > 0 {
			return time.Duration(sec) * time.Second
		}
	}
	return imDefaultAttachmentParseWait
}

type imParseOutcome struct {
	result *types.ReadResult
	err    error
}

// readIMAttachmentWithEngine reads an IM attachment with the engine the chat
// attachment rules picked, given the workspace's parser settings as the web
// chat passes them. It returns nil, and the caller falls back to the default
// reader, when the engine cannot be built, fails, reports an error in its
// result, finds no text, or does not finish within imAttachmentParseWait. The
// reason goes to the log, never to the user.
//
// The read runs in its own goroutine because not every reader stops when its
// context ends (the docreader client and parts of the PaddleOCR-VL cloud
// client do not): the reply must not wait for them. A reader still running
// after the wait ends on its own; its result is dropped.
func (s *Service) readIMAttachmentWithEngine(
	ctx context.Context, engine string, base *types.ReadRequest, agent *types.CustomAgent, tenant *types.Tenant,
) *types.ReadResult {
	request := *base
	request.ParserEngine = engine
	if tenant != nil && tenant.ParserEngineConfig != nil {
		request.ParserEngineOverrides = tenant.ParserEngineConfig.ToOverridesMap()
	}
	deps := docparser.ReaderDeps{Overrides: request.ParserEngineOverrides}
	if s.documentReader != nil {
		deps.Remote = s.documentReader
	}
	if s.tenantService != nil {
		deps.WeKnoraCloudCredentials = s.tenantService.GetWeKnoraCloudCredentials
	}

	wait := imAttachmentParseWait(agent)
	parseCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	reader, err := docparser.NewReader(parseCtx, engine, request.FileType, false, deps)
	if err != nil {
		logger.Warnf(ctx, "[IM] attachment parser %q cannot read .%s, using the default reader: %v",
			engine, request.FileType, err)
		return nil
	}

	started := time.Now()
	done := make(chan imParseOutcome, 1)
	go func() {
		result, err := reader.Read(parseCtx, &request)
		done <- imParseOutcome{result: result, err: err}
	}()
	var outcome imParseOutcome
	select {
	case outcome = <-done:
	case <-parseCtx.Done():
		if errors.Is(parseCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			logger.Warnf(ctx, "[IM] attachment parser %q did not finish .%s within %s, using the default reader",
				engine, request.FileType, wait)
		}
		return nil
	}

	switch {
	case outcome.err != nil:
		logger.Warnf(ctx, "[IM] attachment parser %q failed on .%s, using the default reader: %v",
			engine, request.FileType, outcome.err)
		return nil
	case outcome.result == nil:
		logger.Warnf(ctx, "[IM] attachment parser %q returned nothing for .%s, using the default reader",
			engine, request.FileType)
		return nil
	case strings.TrimSpace(outcome.result.Error) != "":
		logger.Warnf(ctx, "[IM] attachment parser %q failed on .%s, using the default reader: %s",
			engine, request.FileType, outcome.result.Error)
		return nil
	case strings.TrimSpace(outcome.result.MarkdownContent) == "":
		logger.Warnf(ctx, "[IM] attachment parser %q found no text in .%s, using the default reader",
			engine, request.FileType)
		return nil
	}
	outcome.result.MarkdownContent = docparser.NormalizeHTMLTables(outcome.result.MarkdownContent)
	logger.Infof(ctx, "[IM] attachment parsed: engine=%s type=%s chars=%d took=%s",
		engine, request.FileType, utf8.RuneCountInString(outcome.result.MarkdownContent),
		time.Since(started).Round(time.Millisecond))
	return outcome.result
}

// readIMAttachmentDefault is how IM read attachments before it followed the
// chat attachment rules, and still does when no rule or default engine applies
// or the chosen engine gives up: images and complex formats go to docreader
// with no engine named, simple formats to the Go reader. A failure is logged
// and leaves only the attachment's metadata.
func (s *Service) readIMAttachmentDefault(
	ctx context.Context, request *types.ReadRequest, fileType string, isImage bool,
) *types.ReadResult {
	var result *types.ReadResult
	var err error
	if isImage && s.documentReader != nil {
		result, err = s.documentReader.Read(ctx, request)
		if err != nil {
			logger.Warnf(ctx, "[IM] image OCR/document parsing failed, continuing with vision input: %v", err)
			result, err = nil, nil
		}
	}
	if result == nil && docparser.IsSimpleFormat(fileType) {
		result, err = (&docparser.SimpleFormatReader{}).Read(ctx, request)
	} else if result == nil && !isImage && s.documentReader != nil {
		result, err = s.documentReader.Read(ctx, request)
	}
	if err != nil {
		logger.Warnf(ctx, "[IM] attachment parsing failed, continuing with attachment metadata: %v", err)
	}
	return result
}
