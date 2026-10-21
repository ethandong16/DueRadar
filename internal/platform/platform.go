// Package platform defines the boundary for independently verified adapters.
package platform

import (
	"context"
	"errors"
	"github.com/ethandong16/due-radar/internal/core"
)

var ErrNotValidated = errors.New("平台登录与任务接口待实测，尚未启用采集")

type Adapter interface {
	// Validate must prove authentication independently of task collection.
	Validate(context.Context) error
	// Tasks returns platform-confirmed values only; missing times stay nil.
	Tasks(context.Context) ([]core.Task, error)
}

type Capability struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Ready   bool   `json:"ready"`
	Message string `json:"message"`
}

func Capabilities() []Capability {
	return []Capability{
		{ID: core.PlatformRain, Name: "普通雨课堂", Message: ErrNotValidated.Error()},
		{ID: core.PlatformZhidaoShared, Name: "知到 · 共享课", Message: ErrNotValidated.Error()},
		{ID: core.PlatformZhidaoCampus, Name: "知到 · 校内课", Message: ErrNotValidated.Error()},
	}
}
