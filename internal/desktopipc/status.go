package desktopipc

import "time"

// Status is a detached worker snapshot. Session ownership comes from the
// parent process's request, never from whichever session is selected in the UI.
type Status struct {
	SubAgents       int        `json:"subAgents"`
	NamedAgents     int        `json:"namedAgents"`
	BackgroundTasks int        `json:"backgroundTasks"`
	Agents          []Subagent `json:"agents"`
	Jobs            []Job      `json:"jobs"`
}

type Subagent struct {
	Name            string    `json:"name"`
	AgentID         string    `json:"agentId"`
	Anonymous       bool      `json:"anonymous"`
	Status          string    `json:"status"`
	Background      bool      `json:"background"`
	StartedAt       time.Time `json:"startedAt"`
	EndedAt         time.Time `json:"endedAt"`
	ElapsedMS       int64     `json:"elapsedMs"`
	Output          string    `json:"output"`
	OutputTruncated bool      `json:"outputTruncated"`
	Result          string    `json:"result"`
	ResultTruncated bool      `json:"resultTruncated"`
	StopHint        string    `json:"stopHint"`
	ExitError       string    `json:"exitError"`
}

type Job struct {
	ID          string    `json:"id"`
	Description string    `json:"description"`
	Status      string    `json:"status"`
	StartedAt   time.Time `json:"startedAt"`
}
