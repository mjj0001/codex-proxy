/**
 * 多号轮询选择器模块
 * 实现 Round-Robin 和 Fill-First 两种账号选择策略
 * 支持优先级、冷却检测和并发安全
 */
package auth

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

/**
 * Selector 定义账号选择器接口
 * @method Pick - 从可用账号中选择一个
 */
type Selector interface {
	Pick(model string, accounts []*Account) (*Account, error)
}

/**
 * RoundRobinSelector 轮询选择器
 * 按顺序在可用账号之间循环选择，确保请求均匀分布
 * @field mu - 并发保护锁
 * @field cursors - 每个模型的当前游标位置
 */
type RoundRobinSelector struct {
	mu      sync.Mutex
	cursors map[string]int
}

/**
 * NewRoundRobinSelector 创建新的轮询选择器
 * @returns *RoundRobinSelector - 轮询选择器实例
 */
func NewRoundRobinSelector() *RoundRobinSelector {
	return &RoundRobinSelector{
		cursors: make(map[string]int),
	}
}

/**
 * Pick 使用轮询策略选择下一个可用账号
 * @param model - 请求的模型名称
 * @param accounts - 全部账号列表
 * @returns *Account - 选中的账号
 * @returns error - 没有可用账号时返回错误
 */
func (s *RoundRobinSelector) Pick(model string, accounts []*Account) (*Account, error) {
	available := filterAvailable(accounts)
	if len(available) == 0 {
		return nil, fmt.Errorf("没有可用的 Codex 账号")
	}

	/*
	 * 按额度使用率升序排序（剩余额度最多的优先）
	 * used_percent: 0=最空闲, 100=已满, -1=未知（排最后）
	 * 同使用率时按文件路径保持稳定顺序
	 */
	sort.Slice(available, func(i, j int) bool {
		pi := available[i].GetUsedPercent()
		pj := available[j].GetUsedPercent()
		if pi < 0 && pj >= 0 {
			return false
		}
		if pi >= 0 && pj < 0 {
			return true
		}
		if pi != pj {
			return pi < pj
		}
		return available[i].FilePath < available[j].FilePath
	})

	s.mu.Lock()
	defer s.mu.Unlock()

	key := "codex:" + model
	index := s.cursors[key]

	if index >= 2_147_483_640 {
		index = 0
	}

	selected := available[index%len(available)]
	s.cursors[key] = index + 1

	return selected, nil
}

/**
 * FillFirstSelector 填充优先选择器
 * 始终优先使用第一个可用账号，直到该账号进入冷却后再切换
 * 适合需要消耗单个账号配额上限的场景
 */
type FillFirstSelector struct{}

/**
 * NewFillFirstSelector 创建新的填充优先选择器
 * @returns *FillFirstSelector - 填充优先选择器实例
 */
func NewFillFirstSelector() *FillFirstSelector {
	return &FillFirstSelector{}
}

/**
 * Pick 使用填充优先策略选择账号
 * @param model - 请求的模型名称
 * @param accounts - 全部账号列表
 * @returns *Account - 选中的账号
 * @returns error - 没有可用账号时返回错误
 */
func (s *FillFirstSelector) Pick(model string, accounts []*Account) (*Account, error) {
	available := filterAvailable(accounts)
	if len(available) == 0 {
		return nil, fmt.Errorf("没有可用的 Codex 账号")
	}

	/* 按文件路径排序后返回第一个 */
	sort.Slice(available, func(i, j int) bool {
		return available[i].FilePath < available[j].FilePath
	})

	return available[0], nil
}

/**
 * filterAvailable 过滤出当前可用的账号
 * @param accounts - 全部账号列表
 * @returns []*Account - 可用的账号列表
 */
func filterAvailable(accounts []*Account) []*Account {
	now := time.Now()
	available := make([]*Account, 0, len(accounts))

	for _, acc := range accounts {
		acc.mu.RLock()
		status := acc.Status
		cooldownUntil := acc.CooldownUntil
		acc.mu.RUnlock()

		switch status {
		case StatusDisabled:
			continue
		case StatusCooldown:
			if now.Before(cooldownUntil) {
				continue
			}
		}

		available = append(available, acc)
	}

	return available
}
