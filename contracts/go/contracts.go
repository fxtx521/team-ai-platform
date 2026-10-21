// Package contracts — 契约即法律（AGENTS.md）。
// Go 执行器与服务端共享的路径/错误码/状态机，与 contracts/schema.py 一一对应，改动必须双侧同步。
package contracts

const (
	PathAuthToken    = "/auth/token"
	PathHeartbeat    = "/heartbeat"
	PathTasks        = "/tasks"
	PathInboxPull    = "/inbox/pull"
	PathSkills       = "/skills"
	PathModelRoute   = "/model/route"
)

const (
	CodeAuthFailed   = 40101
	CodeTokenExpired = 40102
	CodePathEscape   = 40301
	CodeRateLimited  = 42901
)

// JobStatus 状态机：queued -> running -> done | failed | cancelled
type JobStatus string

const (
	JobQueued    JobStatus = "queued"
	JobRunning   JobStatus = "running"
	JobDone      JobStatus = "done"
	JobFailed    JobStatus = "failed"
	JobCancelled JobStatus = "cancelled"
)
