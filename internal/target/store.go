package target

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"olt-diagnostic-agent/internal/domain"
)

// Store 是 target profile 的内存存储：以 profile ID 为键，用读写锁保证并发安全。
type Store struct {
	mu       sync.RWMutex
	profiles map[string]domain.TargetProfile
}

// NewStore 创建空的 Store。
func NewStore() *Store {
	return &Store{profiles: make(map[string]domain.TargetProfile)}
}

func (s *Store) Save(profile domain.TargetProfile) error {
	profile.ID = strings.TrimSpace(profile.ID)
	profile.Name = strings.TrimSpace(profile.Name)
	profile.OrganizationCode = strings.ToUpper(strings.TrimSpace(profile.OrganizationCode))
	if profile.OrganizationCode == "" {
		profile.OrganizationCode = "A01"
	}
	if profile.ID == "" {
		return errors.New("profile ID is required")
	}
	if profile.Name == "" {
		return errors.New("profile name is required")
	}
	if profile.NBI == nil && profile.NETCONF == nil && len(profile.NETCONFEndpoints) == 0 {
		return errors.New("profile must contain an NBI or NETCONF target")
	}
	if profile.NBI != nil && strings.TrimSpace(profile.NBI.BaseURL) == "" {
		return errors.New("NBI base URL is required")
	}
	if profile.NBI != nil {
		baseURL, err := url.Parse(strings.TrimSpace(profile.NBI.BaseURL))
		if err != nil || baseURL.Host == "" || (baseURL.Scheme != "http" && baseURL.Scheme != "https") {
			return errors.New("NBI base URL must be an HTTP or HTTPS URL with a host")
		}
		if profile.NBI.InsecureTLS && strings.TrimSpace(profile.NBI.CAFile) != "" {
			return errors.New("NBI custom CA and insecure TLS mode cannot be enabled together")
		}
		profile.NBI.Username = strings.TrimSpace(profile.NBI.Username)
		if (profile.NBI.Username == "") != (profile.NBI.Password == "") {
			return errors.New("NBI username and password must be configured together")
		}
		if profile.NBI.Username == "" && strings.TrimSpace(profile.NBI.Token) == "" {
			return errors.New("NBI username and password or a static token are required")
		}
		profile.NBI.BaseURL = strings.TrimRight(baseURL.String(), "/")
	}
	if profile.NETCONF != nil {
		if err := validateNETCONFSettings(
			&profile.NETCONF.Address,
			&profile.NETCONF.Port,
			&profile.NETCONF.Username,
			profile.NETCONF.Password,
			profile.NETCONF.KnownHostsFile,
			profile.NETCONF.InsecureHostKey,
			"NETCONF",
		); err != nil {
			return err
		}
	}
	seenEndpoints := make(map[string]struct{}, len(profile.NETCONFEndpoints))
	for index := range profile.NETCONFEndpoints {
		endpoint := &profile.NETCONFEndpoints[index]
		endpoint.ID = strings.TrimSpace(endpoint.ID)
		if endpoint.ID == "" {
			endpoint.ID = fmt.Sprintf("endpoint-%d", index+1)
		}
		endpoint.Name = strings.TrimSpace(endpoint.Name)
		if endpoint.Name == "" {
			endpoint.Name = endpoint.ID
		}
		key := strings.ToLower(endpoint.ID)
		if _, exists := seenEndpoints[key]; exists {
			return fmt.Errorf("duplicate NETCONF endpoint ID: %s", endpoint.ID)
		}
		seenEndpoints[key] = struct{}{}
		if err := validateNETCONFSettings(
			&endpoint.Address,
			&endpoint.Port,
			&endpoint.Username,
			endpoint.Password,
			endpoint.KnownHostsFile,
			endpoint.InsecureHostKey,
			fmt.Sprintf("NETCONF endpoint %q", endpoint.ID),
		); err != nil {
			return err
		}
	}
	for index, root := range profile.WorkspaceRoots {
		absoluteRoot, err := filepath.Abs(strings.TrimSpace(root))
		if err != nil {
			return fmt.Errorf("resolve workspace root: %w", err)
		}
		info, err := os.Stat(absoluteRoot)
		if err != nil || !info.IsDir() {
			return fmt.Errorf("workspace root is not an accessible directory: %s", absoluteRoot)
		}
		canonicalRoot, err := filepath.EvalSymlinks(absoluteRoot)
		if err != nil {
			return fmt.Errorf("resolve workspace root links: %w", err)
		}
		profile.WorkspaceRoots[index] = canonicalRoot
	}
	for index, skillPath := range profile.SkillPaths {
		// 校验受信 skill 文件：必须位于某 workspace root 内、文件名为 SKILL.md、可读且不超过 128 KiB。
		absoluteSkill, err := filepath.Abs(strings.TrimSpace(skillPath))
		if err != nil {
			return fmt.Errorf("resolve skill path: %w", err)
		}
		canonicalSkill, err := filepath.EvalSymlinks(absoluteSkill)
		if err != nil {
			return fmt.Errorf("resolve skill path links: %w", err)
		}
		if !pathInsideRoots(canonicalSkill, profile.WorkspaceRoots) {
			return fmt.Errorf("skill path is outside the configured workspace roots: %s", skillPath)
		}
		if !strings.EqualFold(filepath.Base(canonicalSkill), "SKILL.md") {
			return fmt.Errorf("trusted skill file must be named SKILL.md: %s", skillPath)
		}
		info, err := os.Stat(canonicalSkill)
		if err != nil || info.IsDir() {
			return fmt.Errorf("skill path is not a readable file: %s", skillPath)
		}
		if info.Size() > 128*1024 {
			return fmt.Errorf("skill file exceeds 128 KiB: %s", skillPath)
		}
		profile.SkillPaths[index] = canonicalSkill
	}
	// 深拷贝切片与指针字段后再存入，防止调用方持有的引用影响已保存的数据。
	profile.WorkspaceRoots = append([]string(nil), profile.WorkspaceRoots...)
	profile.SkillPaths = append([]string(nil), profile.SkillPaths...)
	if profile.NBI != nil {
		nbi := *profile.NBI
		profile.NBI = &nbi
	}
	if profile.NETCONF != nil {
		netconf := *profile.NETCONF
		profile.NETCONF = &netconf
	}
	profile.NETCONFEndpoints = append([]domain.NETCONFEndpoint(nil), profile.NETCONFEndpoints...)

	s.mu.Lock()
	s.profiles[profile.ID] = profile
	s.mu.Unlock()
	return nil
}

// validateNETCONFSettings 校验并规范化一段 NETCONF 连接配置：必须有地址、
// 用户名/密码与合法端口（缺省 830）；非 insecure 模式时还必须提供 known-hosts 文件。
// label 用于错误信息中标识是单点 NETCONF 还是某个 endpoint。
func validateNETCONFSettings(address *string, port *int, username *string, password, knownHostsFile string, insecureHostKey bool, label string) error {
	*address = strings.TrimSpace(*address)
	if *address == "" {
		return fmt.Errorf("%s address is required", label)
	}
	if *port == 0 {
		*port = 830
	}
	if *port < 1 || *port > 65535 {
		return fmt.Errorf("%s port must be between 1 and 65535", label)
	}
	*username = strings.TrimSpace(*username)
	if *username == "" || password == "" {
		return fmt.Errorf("%s username and password are required", label)
	}
	if !insecureHostKey && strings.TrimSpace(knownHostsFile) == "" {
		return fmt.Errorf("%s known-hosts file is required unless insecure host-key mode is enabled", label)
	}
	return nil
}

func (s *Store) WorkspaceRoots(profileID string) ([]string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	profile, exists := s.profiles[profileID]
	if !exists {
		return nil, false
	}
	return append([]string(nil), profile.WorkspaceRoots...), true
}

// Skills 读取并返回 profile 所有受信 skill 文件的内容（单文件上限 128 KiB，读取出错时跳过该文件）。
func (s *Store) Skills(profileID string) ([]string, bool) {
	s.mu.RLock()
	profile, exists := s.profiles[profileID]
	paths := append([]string(nil), profile.SkillPaths...)
	s.mu.RUnlock()
	if !exists {
		return nil, false
	}
	skills := make([]string, 0, len(paths))
	for _, path := range paths {
		file, err := os.Open(path)
		if err != nil {
			continue
		}
		content, readErr := io.ReadAll(io.LimitReader(file, 128*1024+1))
		_ = file.Close()
		if readErr == nil && len(content) <= 128*1024 {
			skills = append(skills, string(content))
		}
	}
	return skills, true
}

func (s *Store) NBI(profileID string) (domain.NBITarget, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	profile, exists := s.profiles[profileID]
	if !exists || profile.NBI == nil {
		return domain.NBITarget{}, false
	}
	return *profile.NBI, true
}

func (s *Store) NETCONF(profileID string) (domain.NETCONFTarget, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	profile, exists := s.profiles[profileID]
	if !exists {
		return domain.NETCONFTarget{}, false
	}
	if profile.NETCONF != nil {
		return *profile.NETCONF, true
	}
	if len(profile.NETCONFEndpoints) > 0 {
		return netconfTargetFromEndpoint(profile.NETCONFEndpoints[0]), true
	}
	return domain.NETCONFTarget{}, false
}

// NETCONFEndpoints 返回 profile 的全部 NETCONF endpoint；只有单点配置时合成一个 "default" endpoint。
func (s *Store) NETCONFEndpoints(profileID string) ([]domain.NETCONFEndpoint, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	profile, exists := s.profiles[profileID]
	if !exists {
		return nil, false
	}
	if len(profile.NETCONFEndpoints) > 0 {
		return append([]domain.NETCONFEndpoint(nil), profile.NETCONFEndpoints...), true
	}
	if profile.NETCONF == nil {
		return nil, false
	}
	return []domain.NETCONFEndpoint{{
		ID:              "default",
		Name:            "default",
		Address:         profile.NETCONF.Address,
		Port:            profile.NETCONF.Port,
		Username:        profile.NETCONF.Username,
		Password:        profile.NETCONF.Password,
		KnownHostsFile:  profile.NETCONF.KnownHostsFile,
		InsecureHostKey: profile.NETCONF.InsecureHostKey,
	}}, true
}

// Exists 判断指定 ID 的 profile 是否已保存。
func (s *Store) Exists(profileID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, exists := s.profiles[profileID]
	return exists
}

func (s *Store) Profile(profileID string) (domain.TargetProfile, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	stored, exists := s.profiles[profileID]
	if !exists {
		return domain.TargetProfile{}, false
	}
	return cloneProfile(stored), true
}

func (s *Store) Summaries() []domain.TargetProfileSummary {
	s.mu.RLock()
	defer s.mu.RUnlock()

	summaries := make([]domain.TargetProfileSummary, 0, len(s.profiles))
	for _, profile := range s.profiles {
		summary := domain.TargetProfileSummary{ID: profile.ID, Name: profile.Name, OrganizationCode: profile.OrganizationCode}
		summary.WorkspaceRoots = append([]string(nil), profile.WorkspaceRoots...)
		summary.SkillPaths = append([]string(nil), profile.SkillPaths...)
		if profile.NBI != nil {
			summary.NBIBaseURL = profile.NBI.BaseURL
			summary.OLTAddress = profile.NBI.OLTAddress
			summary.NBIUsername = profile.NBI.Username
			summary.TokenHeader = profile.NBI.TokenHeader
			summary.CAFile = profile.NBI.CAFile
			summary.InsecureTLS = profile.NBI.InsecureTLS
			summary.NBITokenConfigured = profile.NBI.Token != ""
			summary.NBIPasswordConfigured = profile.NBI.Password != ""
		}
		if profile.NETCONF != nil {
			summary.NETCONFAddress = profile.NETCONF.Address
			summary.NETCONFPort = profile.NETCONF.Port
			summary.NETCONFUsername = profile.NETCONF.Username
			summary.KnownHostsFile = profile.NETCONF.KnownHostsFile
			summary.InsecureHostKey = profile.NETCONF.InsecureHostKey
			summary.NETCONFPasswordConfigured = profile.NETCONF.Password != ""
		}
		endpoints := profile.NETCONFEndpoints
		if len(endpoints) == 0 && profile.NETCONF != nil {
			endpoints = []domain.NETCONFEndpoint{{
				ID:              "default",
				Name:            "default",
				Address:         profile.NETCONF.Address,
				Port:            profile.NETCONF.Port,
				Username:        profile.NETCONF.Username,
				Password:        profile.NETCONF.Password,
				KnownHostsFile:  profile.NETCONF.KnownHostsFile,
				InsecureHostKey: profile.NETCONF.InsecureHostKey,
			}}
		}
		for _, endpoint := range endpoints {
			if summary.NETCONFAddress == "" || len(profile.NETCONFEndpoints) > 0 && len(summary.NETCONFEndpoints) == 0 {
				summary.NETCONFAddress = endpoint.Address
				summary.NETCONFPort = endpoint.Port
				summary.NETCONFUsername = endpoint.Username
				summary.KnownHostsFile = endpoint.KnownHostsFile
				summary.InsecureHostKey = endpoint.InsecureHostKey
				summary.NETCONFPasswordConfigured = endpoint.Password != ""
			}
			summary.NETCONFEndpoints = append(summary.NETCONFEndpoints, domain.NETCONFEndpointSummary{
				ID:                 endpoint.ID,
				Name:               endpoint.Name,
				Address:            endpoint.Address,
				Port:               endpoint.Port,
				Username:           endpoint.Username,
				KnownHostsFile:     endpoint.KnownHostsFile,
				InsecureHostKey:    endpoint.InsecureHostKey,
				PasswordConfigured: endpoint.Password != "",
			})
		}
		summaries = append(summaries, summary)
	}
	sort.Slice(summaries, func(i, j int) bool {
		return summaries[i].Name < summaries[j].Name
	})
	return summaries
}

func (s *Store) Profiles() []domain.TargetProfile {
	s.mu.RLock()
	defer s.mu.RUnlock()

	profiles := make([]domain.TargetProfile, 0, len(s.profiles))
	for _, stored := range s.profiles {
		profiles = append(profiles, cloneProfile(stored))
	}
	sort.Slice(profiles, func(i, j int) bool {
		return profiles[i].Name < profiles[j].Name
	})
	return profiles
}

// cloneProfile 深拷贝一个 profile，断开切片与指针字段与存储数据的别名。
func cloneProfile(stored domain.TargetProfile) domain.TargetProfile {
	profile := stored
	profile.WorkspaceRoots = append([]string(nil), stored.WorkspaceRoots...)
	profile.SkillPaths = append([]string(nil), stored.SkillPaths...)
	if stored.NBI != nil {
		nbi := *stored.NBI
		profile.NBI = &nbi
	}
	if stored.NETCONF != nil {
		netconf := *stored.NETCONF
		profile.NETCONF = &netconf
	}
	profile.NETCONFEndpoints = append([]domain.NETCONFEndpoint(nil), stored.NETCONFEndpoints...)
	return profile
}

// netconfTargetFromEndpoint 将 endpoint 字段映射为单点 NETCONF target。
func netconfTargetFromEndpoint(endpoint domain.NETCONFEndpoint) domain.NETCONFTarget {
	return domain.NETCONFTarget{
		Address:         endpoint.Address,
		Port:            endpoint.Port,
		Username:        endpoint.Username,
		Password:        endpoint.Password,
		KnownHostsFile:  endpoint.KnownHostsFile,
		InsecureHostKey: endpoint.InsecureHostKey,
	}
}

// pathInsideRoots 判断 path 是否位于任一 root 之内（基于相对路径前缀判断）。
func pathInsideRoots(path string, roots []string) bool {
	for _, root := range roots {
		relative, err := filepath.Rel(root, path)
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative) {
			return true
		}
	}
	return false
}
