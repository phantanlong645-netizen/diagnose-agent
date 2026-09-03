package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"olt-diagnostic-agent/internal/domain"
)

type NETCONFRPCRequest struct {
	ProfileID      string `json:"profileId"`
	Endpoint       string `json:"endpoint,omitempty"`
	RPC            string `json:"rpc"`
	TimeoutSeconds int    `json:"timeoutSeconds,omitempty"`
}

type NETCONFRPCResponse struct {
	SessionID          string `json:"sessionId,omitempty"`
	RequestedMessageID string `json:"requestedMessageId,omitempty"`
	MessageID          string `json:"messageId,omitempty"`
	Reply              string `json:"reply"`
	RPCError           string `json:"rpcError,omitempty"`
	DurationMS         int64  `json:"durationMs"`
	CloseAcknowledged  bool   `json:"closeAcknowledged"`
	CloseError         string `json:"closeError,omitempty"`
}

type NETCONFTransport interface {
	ExecuteRPC(ctx context.Context, target domain.NETCONFTarget, rpc string, timeout time.Duration) (NETCONFRPCResponse, error)
}

type NETCONFTool struct {
	targets   TargetResolver
	transport NETCONFTransport
}

func NewNETCONFTool(targets TargetResolver, transport NETCONFTransport) (*NETCONFTool, error) {
	if targets == nil {
		return nil, errors.New("target resolver is required")
	}
	return &NETCONFTool{targets: targets, transport: transport}, nil
}

func (t *NETCONFTool) Definition() Definition {
	return Definition{
		Name:        "netconf_rpc",
		Description: "Execute a NETCONF RPC against a named endpoint of the configured OLT and return the raw RPC reply",
	}
}

func (t *NETCONFTool) Prepare(arguments json.RawMessage) (domain.PreparedCall, error) {
	var input NETCONFRPCRequest
	if err := json.Unmarshal(arguments, &input); err != nil {
		return domain.PreparedCall{}, fmt.Errorf("decode arguments: %w", err)
	}
	input.ProfileID = strings.TrimSpace(input.ProfileID)
	target, endpointID, err := t.netconfTarget(input.ProfileID, input.Endpoint)
	if err != nil {
		return domain.PreparedCall{}, err
	}
	input.RPC = strings.TrimSpace(input.RPC)
	if input.RPC == "" {
		return domain.PreparedCall{}, errors.New("NETCONF RPC is required")
	}
	if input.TimeoutSeconds == 0 {
		input.TimeoutSeconds = 30
	}
	if input.TimeoutSeconds < 1 || input.TimeoutSeconds > 300 {
		return domain.PreparedCall{}, errors.New("NETCONF timeout must be between 1 and 300 seconds")
	}
	operation, err := netconfOperation(input.RPC)
	if err != nil {
		return domain.PreparedCall{}, err
	}
	if (operation == "get" || operation == "get-config") && !netconfRPCHasFilter(input.RPC) {
		return domain.PreparedCall{}, fmt.Errorf("unfiltered NETCONF %s is blocked; provide a narrow subtree filter or use a verified RPC recipe", operation)
	}
	if operation == "get" || operation == "get-config" {
		// 宽 filter 形态检测：filter 内只含"单个自闭合顶层容器"时几乎一定返回
		// 整个 datastore（如 <configure/>、<state/>、<chassis/>），拒绝并提示
		// 模型换具体节点。配置/netconf-rpc 收到的 reply 会很大，耗光 token
		// 并触发不必要的 summarization。
		if wide, reason := netconfFilterIsTooBroad(input.RPC); wide {
			return domain.PreparedCall{}, fmt.Errorf("NETCONF %s filter looks too broad: %s. Reference the matching .tpl recipe or use a specific ONT name, interface name, AID, or YANG leaf path. The runtime rejects top-level containers that would match every descendant.", operation, reason)
		}
	}

	annotations := domain.ToolAnnotations{OpenWorld: true}
	switch operation {
	case "get", "get-config", "get-schema", "validate":
		annotations.ReadOnly = true
		annotations.Idempotent = true
	case "lock", "unlock", "close-session", "kill-session":
		annotations.Idempotent = false
	case "delete-config", "copy-config", "edit-config", "commit", "discard-changes":
		annotations.Destructive = true
	default:
		annotations.Destructive = true
	}

	return domain.PreparedCall{
		Summary:     fmt.Sprintf("NETCONF %s on %s:%d [%s]", operation, target.Address, target.Port, endpointID),
		Preview:     abbreviate(redactNETCONFReply(input.RPC), 12000),
		Annotations: annotations,
		Input: preparedNETCONFRPC{
			Request: input,
			Target:  target,
		},
	}, nil
}

func (t *NETCONFTool) Execute(ctx context.Context, call domain.PreparedCall) (domain.ToolResult, error) {
	prepared, ok := call.Input.(preparedNETCONFRPC)
	if !ok {
		return domain.ToolResult{}, errors.New("invalid prepared NETCONF RPC")
	}
	if t.transport == nil {
		return domain.ToolResult{}, errors.New("NETCONF transport is not configured")
	}
	input := prepared.Request
	response, err := t.transport.ExecuteRPC(ctx, prepared.Target, input.RPC, time.Duration(input.TimeoutSeconds)*time.Second)
	if err != nil {
		return domain.ToolResult{}, fmt.Errorf("execute NETCONF RPC: %w", err)
	}
	operation, _ := netconfOperation(input.RPC)
	replyEmpty := netconfReplyIsEmpty(response.Reply)
	summary := fmt.Sprintf("NETCONF %s completed", operation)
	if response.RPCError != "" {
		summary = fmt.Sprintf("NETCONF %s returned rpc-error", operation)
	}
	data := domain.ToolResult{
		Summary: summary,
		Data:    response,
		Metadata: map[string]string{
			"netconf.session_id":         response.SessionID,
			"netconf.message_id":         response.MessageID,
			"netconf.operation":          operation,
			"netconf.rpc_error":          strconv.FormatBool(response.RPCError != ""),
			"netconf.reply_empty":        strconv.FormatBool(replyEmpty),
			"netconf.close_acknowledged": strconv.FormatBool(response.CloseAcknowledged),
		},
	}
	messages := make([]string, 0, 2)
	if response.CloseError != "" {
		messages = append(messages, "The business RPC completed, but NETCONF close-session failed: "+response.CloseError+". Do not repeat the business RPC solely because of this cleanup warning.")
	}
	if replyEmpty && (operation == "get" || operation == "get-config") {
		// 空 <data/> 时给模型一段诊断提示，避免反复试错。
		// 提示内容与 builtin/skill.md 的空数据诊断流程一致。
		netconfDiagnosticHint := netconfEmptyDataHint(input.Endpoint, operation)
		messages = append(messages, netconfDiagnosticHint)
	}
	if len(messages) > 0 {
		data.Message = strings.Join(messages, "\n")
	}
	return data, nil
}

type SSHNETCONFTransport struct{}

func NewSSHNETCONFTransport() *SSHNETCONFTransport {
	return &SSHNETCONFTransport{}
}

func (t *SSHNETCONFTransport) ExecuteRPC(
	ctx context.Context,
	target domain.NETCONFTarget,
	rpc string,
	timeout time.Duration,
) (NETCONFRPCResponse, error) {
	select {
	case <-ctx.Done():
		return NETCONFRPCResponse{}, ctx.Err()
	default:
	}

	var envelope struct {
		XMLName   xml.Name `xml:"rpc"`
		MessageID string   `xml:"message-id,attr"`
		Payload   string   `xml:",innerxml"`
	}
	if err := xml.Unmarshal([]byte(rpc), &envelope); err != nil {
		return NETCONFRPCResponse{}, fmt.Errorf("decode NETCONF RPC envelope: %w", err)
	}
	if envelope.XMLName.Local != "rpc" || strings.TrimSpace(envelope.Payload) == "" {
		return NETCONFRPCResponse{}, errors.New("NETCONF RPC must contain an operation")
	}

	var hostKeyCallback ssh.HostKeyCallback
	if target.InsecureHostKey {
		hostKeyCallback = ssh.InsecureIgnoreHostKey() //nolint:gosec -- explicit per-target lab setting
	} else if strings.TrimSpace(target.KnownHostsFile) != "" {
		var err error
		hostKeyCallback, err = knownhosts.New(target.KnownHostsFile)
		if err != nil {
			return NETCONFRPCResponse{}, fmt.Errorf("load NETCONF known-hosts file: %w", err)
		}
	} else {
		return NETCONFRPCResponse{}, errors.New("NETCONF known-hosts file is required unless insecure host-key mode is enabled")
	}

	address := net.JoinHostPort(target.Address, strconv.Itoa(target.Port))
	dialer := &net.Dialer{Timeout: timeout}
	connection, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return NETCONFRPCResponse{}, fmt.Errorf("connect to NETCONF server: %w", err)
	}
	defer connection.Close()
	deadline := time.Now().Add(timeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if err = connection.SetDeadline(deadline); err != nil {
		return NETCONFRPCResponse{}, fmt.Errorf("set NETCONF deadline: %w", err)
	}
	stopCancellationWatch := make(chan struct{})
	defer close(stopCancellationWatch)
	go func() {
		select {
		case <-ctx.Done():
			_ = connection.SetDeadline(time.Now())
		case <-stopCancellationWatch:
		}
	}()

	startedAt := time.Now()
	clientConnection, channels, requests, err := ssh.NewClientConn(connection, address, &ssh.ClientConfig{
		User:            target.Username,
		Auth:            []ssh.AuthMethod{ssh.Password(target.Password), ssh.KeyboardInteractive(passwordChallenge(target.Password))},
		HostKeyCallback: hostKeyCallback,
		Timeout:         timeout,
	})
	if err != nil {
		return NETCONFRPCResponse{}, fmt.Errorf("establish NETCONF SSH connection: %w", err)
	}
	client := ssh.NewClient(clientConnection, channels, requests)
	defer client.Close()
	session, err := client.NewSession()
	if err != nil {
		return NETCONFRPCResponse{}, fmt.Errorf("create NETCONF SSH session: %w", err)
	}
	defer session.Close()
	input, err := session.StdinPipe()
	if err != nil {
		return NETCONFRPCResponse{}, fmt.Errorf("open NETCONF input: %w", err)
	}
	output, err := session.StdoutPipe()
	if err != nil {
		return NETCONFRPCResponse{}, fmt.Errorf("open NETCONF output: %w", err)
	}
	if err = session.RequestSubsystem("netconf"); err != nil {
		return NETCONFRPCResponse{}, fmt.Errorf("request NETCONF subsystem: %w", err)
	}

	reader := bufio.NewReader(output)
	if _, err = io.WriteString(input, netconfClientHello); err != nil {
		return NETCONFRPCResponse{}, fmt.Errorf("send NETCONF hello: %w", err)
	}
	serverHello, err := readNETCONF10Message(reader, maxNETCONFHelloBytes)
	if err != nil {
		return NETCONFRPCResponse{}, fmt.Errorf("read NETCONF hello: %w", err)
	}
	version := "1.0"
	if bytes.Contains(serverHello, []byte("urn:ietf:params:netconf:base:1.1")) {
		version = "1.1"
	}
	if err = writeNETCONFMessage(input, []byte(rpc), version); err != nil {
		return NETCONFRPCResponse{}, fmt.Errorf("send NETCONF RPC: %w", err)
	}
	var reply []byte
	if version == "1.1" {
		reply, err = readNETCONF11Message(reader, maxNETCONFReplyBytes)
	} else {
		reply, err = readNETCONF10Message(reader, maxNETCONFReplyBytes)
	}
	if err != nil {
		return NETCONFRPCResponse{}, fmt.Errorf("read NETCONF RPC reply: %w", err)
	}
	replyText := strings.TrimSpace(string(reply))

	result := NETCONFRPCResponse{
		SessionID:          netconfSessionID(string(serverHello)),
		RequestedMessageID: envelope.MessageID,
		MessageID:          replyMessageID(replyText),
		Reply:              redactNETCONFReply(replyText),
		DurationMS:         time.Since(startedAt).Milliseconds(),
	}
	if strings.Contains(replyText, "<rpc-error") || strings.Contains(replyText, ":rpc-error") {
		result.RPCError = redactNETCONFReply(replyText)
	}
	operation, operationErr := netconfOperation(rpc)
	if operationErr != nil {
		result.CloseError = fmt.Sprintf("identify completed NETCONF operation: %v", operationErr)
		return result, nil
	}
	if operation == "close-session" {
		result.CloseAcknowledged = result.RPCError == "" && netconfReplyHasOK(reply)
		if !result.CloseAcknowledged && result.RPCError == "" {
			result.CloseError = "close-session reply did not contain <ok/>"
		}
		return result, nil
	}

	closeDeadline := time.Now().Add(netconfCloseSessionTimeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(closeDeadline) {
		closeDeadline = contextDeadline
	}
	if err = connection.SetDeadline(closeDeadline); err != nil {
		result.CloseError = fmt.Sprintf("set close-session deadline: %v", err)
		return result, nil
	}
	if closeErr := closeNETCONFSession(input, reader, version); closeErr != nil {
		result.CloseError = closeErr.Error()
		return result, nil
	}
	result.CloseAcknowledged = true
	return result, nil
}

const (
	maxNETCONFHelloBytes       = 2 * 1024 * 1024
	maxNETCONFReplyBytes       = 16 * 1024 * 1024
	netconfCloseSessionTimeout = 5 * time.Second
	netconfCloseMessageID      = "diagnostic-close-session"
	netconf10Delimiter         = "]]>]]>"
	netconfClientHello         = `<?xml version="1.0" encoding="UTF-8"?>
<hello xmlns="urn:ietf:params:xml:ns:netconf:base:1.0">
  <capabilities>
    <capability>urn:ietf:params:netconf:base:1.0</capability>
    <capability>urn:ietf:params:netconf:base:1.1</capability>
  </capabilities>
</hello>` + netconf10Delimiter
)

func closeNETCONFSession(writer io.Writer, reader *bufio.Reader, version string) error {
	payload := []byte(`<?xml version="1.0" encoding="UTF-8"?><rpc xmlns="urn:ietf:params:xml:ns:netconf:base:1.0" message-id="` + netconfCloseMessageID + `"><close-session/></rpc>`)
	if err := writeNETCONFMessage(writer, payload, version); err != nil {
		return fmt.Errorf("send NETCONF close-session: %w", err)
	}
	reply, err := readNETCONFMessage(reader, version, maxNETCONFReplyBytes)
	if err != nil {
		return fmt.Errorf("read NETCONF close-session reply: %w", err)
	}
	if err = validateNETCONFCloseReply(reply, netconfCloseMessageID); err != nil {
		return err
	}
	return nil
}

func readNETCONFMessage(reader *bufio.Reader, version string, limit int) ([]byte, error) {
	if version == "1.1" {
		return readNETCONF11Message(reader, limit)
	}
	return readNETCONF10Message(reader, limit)
}

func validateNETCONFCloseReply(reply []byte, expectedMessageID string) error {
	var envelope struct {
		XMLName   xml.Name  `xml:"rpc-reply"`
		MessageID string    `xml:"message-id,attr"`
		OK        *struct{} `xml:"ok"`
	}
	if err := xml.Unmarshal(reply, &envelope); err != nil {
		return fmt.Errorf("decode NETCONF close-session reply: %w", err)
	}
	if envelope.XMLName.Local != "rpc-reply" {
		return fmt.Errorf("NETCONF close-session returned <%s> instead of <rpc-reply>", envelope.XMLName.Local)
	}
	if envelope.MessageID != expectedMessageID {
		return fmt.Errorf("NETCONF close-session reply message-id mismatch: got %q, want %q", envelope.MessageID, expectedMessageID)
	}
	if envelope.OK == nil {
		return fmt.Errorf("NETCONF close-session was not acknowledged: %s", abbreviate(redactNETCONFReply(strings.TrimSpace(string(reply))), 1024))
	}
	return nil
}

func netconfReplyHasOK(reply []byte) bool {
	var envelope struct {
		OK *struct{} `xml:"ok"`
	}
	return xml.Unmarshal(reply, &envelope) == nil && envelope.OK != nil
}

func passwordChallenge(password string) ssh.KeyboardInteractiveChallenge {
	return func(_, _ string, questions []string, _ []bool) ([]string, error) {
		answers := make([]string, len(questions))
		for index := range answers {
			answers[index] = password
		}
		return answers, nil
	}
}

func writeNETCONFMessage(writer io.Writer, payload []byte, version string) error {
	if version == "1.1" {
		_, err := fmt.Fprintf(writer, "\n#%d\n%s\n##\n", len(payload), payload)
		return err
	}
	if _, err := writer.Write(payload); err != nil {
		return err
	}
	_, err := io.WriteString(writer, netconf10Delimiter)
	return err
}

func readNETCONF10Message(reader *bufio.Reader, limit int) ([]byte, error) {
	var message bytes.Buffer
	for {
		value, err := reader.ReadByte()
		if err != nil {
			return nil, err
		}
		message.WriteByte(value)
		if message.Len() > limit {
			return nil, errors.New("NETCONF 1.0 message exceeds size limit")
		}
		if bytes.HasSuffix(message.Bytes(), []byte(netconf10Delimiter)) {
			result := message.Bytes()
			return bytes.TrimSpace(result[:len(result)-len(netconf10Delimiter)]), nil
		}
	}
}

func readNETCONF11Message(reader *bufio.Reader, limit int) ([]byte, error) {
	var message bytes.Buffer
	for {
		marker, err := nextNonWhitespaceByte(reader)
		if err != nil {
			return nil, err
		}
		if marker != '#' {
			return nil, fmt.Errorf("invalid NETCONF 1.1 chunk marker: %q", marker)
		}
		first, err := reader.ReadByte()
		if err != nil {
			return nil, err
		}
		if first == '#' {
			if _, err = readLineLimit(reader, 2); err != nil {
				return nil, err
			}
			return bytes.TrimSpace(message.Bytes()), nil
		}
		sizeLine, err := readLineLimit(reader, 20)
		if err != nil {
			return nil, err
		}
		size, err := strconv.Atoi(strings.TrimSpace(string(first) + sizeLine))
		if err != nil || size <= 0 {
			return nil, errors.New("invalid NETCONF 1.1 chunk size")
		}
		if message.Len()+size > limit {
			return nil, errors.New("NETCONF 1.1 message exceeds size limit")
		}
		if _, err = io.CopyN(&message, reader, int64(size)); err != nil {
			return nil, err
		}
	}
}

func readLineLimit(reader *bufio.Reader, limit int) (string, error) {
	var line strings.Builder
	for line.Len() <= limit {
		value, err := reader.ReadByte()
		if err != nil {
			return "", err
		}
		line.WriteByte(value)
		if value == '\n' {
			return line.String(), nil
		}
	}
	return "", errors.New("NETCONF framing line exceeds size limit")
}

func nextNonWhitespaceByte(reader *bufio.Reader) (byte, error) {
	for {
		value, err := reader.ReadByte()
		if err != nil {
			return 0, err
		}
		if value != '\n' && value != '\r' && value != ' ' && value != '\t' {
			return value, nil
		}
	}
}

func netconfSessionID(hello string) string {
	var envelope struct {
		SessionID string `xml:"session-id"`
	}
	if err := xml.Unmarshal([]byte(hello), &envelope); err != nil {
		return ""
	}
	return strings.TrimSpace(envelope.SessionID)
}

// redactNETCONFReply 在保留所有结构的前提下覆盖敏感字段的值。
// 同时把回复用 xml.Encoder 重新序列化（2 空格缩进），让 EVIDENCE INSPECTOR 里显示
// 的 RPC 结果可读；如果保持紧凑一行，单个 16MB 的 YANG 数据就完全无法浏览。
func redactNETCONFReply(reply string) string {
	decoder := xml.NewDecoder(strings.NewReader(reply))
	var output strings.Builder
	encoder := xml.NewEncoder(&output)
	// 两空格缩进是 NETCONF/SROS 配置常见的习惯，让模型在证据里看到整齐缩进
	// 时更容易定位 filter 结构和值。
	encoder.Indent("", "  ")
	sensitiveDepth := 0
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "[NETCONF reply omitted because it could not be safely redacted]"
		}
		switch value := token.(type) {
		case xml.StartElement:
			if sensitiveName(value.Name.Local) {
				sensitiveDepth++
			}
		case xml.EndElement:
			if sensitiveName(value.Name.Local) && sensitiveDepth > 0 {
				sensitiveDepth--
			}
		case xml.CharData:
			if sensitiveDepth > 0 && strings.TrimSpace(string(value)) != "" {
				token = xml.CharData("[REDACTED]")
			}
		}
		if err := encoder.EncodeToken(token); err != nil {
			return "[NETCONF reply omitted because it could not be safely redacted]"
		}
	}
	if err := encoder.Flush(); err != nil {
		return "[NETCONF reply omitted because it could not be safely redacted]"
	}
	return output.String()
}

func replyMessageID(reply string) string {
	var envelope struct {
		MessageID string `xml:"message-id,attr"`
	}
	if err := xml.Unmarshal([]byte(reply), &envelope); err != nil {
		return ""
	}
	return envelope.MessageID
}

type preparedNETCONFRPC struct {
	Request NETCONFRPCRequest
	Target  domain.NETCONFTarget
}

func (t *NETCONFTool) netconfTarget(profileID, endpointID string) (domain.NETCONFTarget, string, error) {
	if profileID == "" {
		return domain.NETCONFTarget{}, "", errors.New("profile ID is required")
	}
	endpoints, exists := t.targets.NETCONFEndpoints(profileID)
	if !exists {
		return domain.NETCONFTarget{}, "", fmt.Errorf("NETCONF target not found for profile: %s", profileID)
	}
	endpointID = strings.TrimSpace(endpointID)
	var selected domain.NETCONFEndpoint
	if endpointID == "" {
		if len(endpoints) != 1 {
			available := make([]string, 0, len(endpoints))
			for _, endpoint := range endpoints {
				available = append(available, endpoint.ID)
			}
			return domain.NETCONFTarget{}, "", fmt.Errorf("NETCONF endpoint is required; choose one of: %s", strings.Join(available, ", "))
		}
		selected = endpoints[0]
	} else {
		for _, endpoint := range endpoints {
			if strings.EqualFold(endpoint.ID, endpointID) || strings.EqualFold(endpoint.Name, endpointID) {
				selected = endpoint
				break
			}
		}
		if selected.ID == "" {
			return domain.NETCONFTarget{}, "", fmt.Errorf("NETCONF endpoint %q was not found for profile %s", endpointID, profileID)
		}
	}
	target := domain.NETCONFTarget{
		Address:         selected.Address,
		Port:            selected.Port,
		Username:        selected.Username,
		Password:        selected.Password,
		KnownHostsFile:  selected.KnownHostsFile,
		InsecureHostKey: selected.InsecureHostKey,
	}
	if strings.TrimSpace(target.Address) == "" {
		return domain.NETCONFTarget{}, "", errors.New("NETCONF target address is required")
	}
	if target.Port == 0 {
		target.Port = 830
	}
	if target.Port < 1 || target.Port > 65535 {
		return domain.NETCONFTarget{}, "", errors.New("NETCONF target port must be between 1 and 65535")
	}
	if strings.TrimSpace(target.Username) == "" {
		return domain.NETCONFTarget{}, "", errors.New("NETCONF username is required")
	}
	if target.Password == "" {
		return domain.NETCONFTarget{}, "", errors.New("NETCONF password is required")
	}
	return target, selected.ID, nil
}

func netconfOperation(rpc string) (string, error) {
	info, err := inspectNETCONFRPC(rpc)
	if err != nil {
		return "", err
	}
	return info.Operation, nil
}

type netconfRPCInfo struct {
	Operation string
	HasFilter bool
}

func netconfRPCHasFilter(rpc string) bool {
	info, err := inspectNETCONFRPC(rpc)
	return err == nil && info.HasFilter
}

func inspectNETCONFRPC(rpc string) (netconfRPCInfo, error) {
	decoder := xml.NewDecoder(strings.NewReader(rpc))
	info := netconfRPCInfo{}
	depth := 0
	rootSeen := false
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return netconfRPCInfo{}, fmt.Errorf("parse NETCONF RPC: %w", err)
		}
		switch value := token.(type) {
		case xml.StartElement:
			if !rootSeen {
				if value.Name.Local != "rpc" {
					return netconfRPCInfo{}, errors.New("NETCONF document root must be rpc")
				}
				rootSeen = true
				depth++
				continue
			}
			if depth == 1 {
				if info.Operation != "" {
					return netconfRPCInfo{}, errors.New("NETCONF RPC must contain exactly one operation")
				}
				info.Operation = value.Name.Local
			} else if depth == 2 && value.Name.Local == "filter" {
				info.HasFilter = true
			}
			depth++
		case xml.EndElement:
			depth--
			if depth < 0 {
				return netconfRPCInfo{}, errors.New("NETCONF RPC has unbalanced XML")
			}
		}
	}
	if !rootSeen {
		return netconfRPCInfo{}, errors.New("NETCONF RPC document is empty")
	}
	if depth != 0 {
		return netconfRPCInfo{}, errors.New("NETCONF RPC has unbalanced XML")
	}
	if info.Operation == "" {
		return netconfRPCInfo{}, errors.New("NETCONF RPC operation is missing")
	}
	return info, nil
}

// netconfReplyIsEmpty 判断 NETCONF 回复是否为空数据。
// 红点后的 reply 格式通常为 <rpc-reply ...><data/></rpc-reply> 或 <rpc-reply ...><data></data></rpc-reply>。
func netconfReplyIsEmpty(reply string) bool {
	trimmed := strings.TrimSpace(reply)
	if trimmed == "" {
		return true
	}
	// <data/> 自闭合空标签
	if strings.Contains(trimmed, "<data/>") {
		return true
	}
	// <data>...</data> 空内容（属性允许，如 <data xmlns="..."></data>）
	dataOpen := regexp.MustCompile(`<data[>\s]`)
	dataClose := regexp.MustCompile(`</data>`)
	if dataOpen.MatchString(trimmed) && dataClose.MatchString(trimmed) {
		start := dataClose.FindStringIndex(trimmed)
		if start != nil {
			// 取 <data...> 结束位置到 </data> 开始位置之间的内容
			openEnd := strings.Index(trimmed[dataOpen.FindStringIndex(trimmed)[1]-1:], ">")
			if openEnd >= 0 {
				inner := trimmed[dataOpen.FindStringIndex(trimmed)[1]+openEnd : start[0]]
				if strings.TrimSpace(inner) == "" {
					return true
				}
			}
		}
	}
	// <ok/> 自闭合
	if strings.Contains(trimmed, "<ok/>") {
		return true
	}
	// <ok></ok> 空内容
	if strings.Contains(trimmed, "<ok></ok>") {
		return true
	}
	return false
}

// netconfEmptyDataHint 返回空数据时的诊断提示，引导模型按正确流程排查。
func netconfEmptyDataHint(endpoint, operation string) string {
	hint := fmt.Sprintf(
		"NETCONF %s returned empty data on endpoint %s. "+
			"The filter did not match any node. Check in order: "+
			"1) Is this the right endpoint for the component? (ihub/nt/lt1/lt2) "+
			"2) Is the namespace correct? Compare against the matching .tpl template under server/internal/pkgs/template_fs/template/olt/. "+
			"3) Is the element path correct? For config, use <get-config> with <source><running/></source>; for state, use <get>. "+
			"4) Broaden the filter progressively — drop one discriminator, move up to the parent container, or use a mid-granularity filter. "+
			"Accepting redundant siblings is preferred over a precise filter that returns empty. "+
			"Do not retry the same RPC unchanged.",
		operation, endpoint,
	)
	return hint
}

// netconfFilterIsTooBroad 检测 filter 是否是"明显过宽"的形态。
//
// 当前规则：filter 顶层只有 1 个 element 且没有任何 child element（也就是
// "单个自闭合顶层容器"，如 <filter type="subtree"><configure/></filter>），
// OLT 几乎一定返回整个 datastore，浪费 token 触发 summarization，直接拒绝。
//
// 已知边界：
//   - <filter><configure><service/></configure></filter> 不被本规则拦截（configure 有 child），
//     但 skill.md L107 已经写明"Do not use a top-level container alone"。
//     模型执意这么写时，会拿到 OLT 的真实返回，由 Execute 阶段的"reply size 兜底"截断。
//   - OLT 厂商的 YANG schema 各异，attribute 限定名(name/id/key)不能穷举；
//     如果有 attribute 限定（如 <interface name="eth-1-1"/>），本规则不会误伤。
func netconfFilterIsTooBroad(rpc string) (bool, string) {
	decoder := xml.NewDecoder(strings.NewReader(rpc))
	inFilter := false
	filterDepth := 0
	var topElements []xml.StartElement
	hasChild := false
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			// 解析失败不归本规则管，让原本的解析错误处理。
			return false, ""
		}
		switch value := token.(type) {
		case xml.StartElement:
			if !inFilter {
				if value.Name.Local == "filter" {
					inFilter = true
					filterDepth = 1
				}
				continue
			}
			filterDepth++
			// filter 顶层子元素：记录
			if filterDepth == 2 {
				topElements = append(topElements, value)
			} else if filterDepth >= 3 {
				// filter 顶层子元素以下还有 element：filter 至少指定了一条具体路径
				hasChild = true
			}
		case xml.EndElement:
			if !inFilter {
				continue
			}
			if value.Name.Local == "filter" {
				switch {
				case len(topElements) == 0:
					return true, "filter has no content; provide at least one container element"
				case len(topElements) == 1 && !hasChild:
					return true, fmt.Sprintf("filter selects only <%s/> as a self-closed top-level container; OLT will return the entire subtree", topElements[0].Name.Local)
				default:
					return false, ""
				}
			}
			filterDepth--
		}
	}
	return false, ""
}
