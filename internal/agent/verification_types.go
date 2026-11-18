package agent

import "time"

// Predicate is an executable assertion over successful, integrity-checked stdout.
// Semantic review is recorded separately: model assessment is not reproduction.
type EvidencePredicate struct {
	Kind     string `json:"kind"`
	Value    string `json:"value,omitempty"`
	Pointer  string `json:"pointer,omitempty"`
	Expected any    `json:"expected,omitempty"`
}

type VerificationRequirement struct {
	ID            string            `json:"id"`
	Description   string            `json:"description"`
	Category      string            `json:"category"` // goal or cleanup
	Predicate     EvidencePredicate `json:"predicate"`
	ToolID        string            `json:"tool_id,omitempty"`
	ArgumentsHash string            `json:"arguments_hash,omitempty"`
	RequireReview bool              `json:"require_review,omitempty"`
}

type EvidenceReference struct {
	ID   string `json:"id"`
	Hash string `json:"hash"`
}

type EvidenceReview struct {
	RequestID   string              `json:"request_id"`
	Status      string              `json:"status"`
	Reason      string              `json:"reason"`
	SubjectHash string              `json:"subject_hash"`
	References  []EvidenceReference `json:"references"`
}

type VerificationCheck struct {
	RequirementID   string              `json:"requirement_id"`
	RequirementHash string              `json:"requirement_hash"`
	EvidenceIDs     []string            `json:"evidence_ids"`
	References      []EvidenceReference `json:"references"`
	Status          string              `json:"status"`
	Reason          string              `json:"reason"`
	Review          *EvidenceReview     `json:"review,omitempty"`
	CreatedAt       time.Time           `json:"created_at"`
}

type Finding struct {
	Report            *ReportText         `json:"report,omitempty"`
	ID                string              `json:"id"`
	Title             string              `json:"title"`
	Severity          string              `json:"severity"`
	RootCause         string              `json:"root_cause"`
	Impact            string              `json:"impact"`
	Remediation       string              `json:"remediation"`
	ReproductionSteps []string            `json:"reproduction_steps"`
	EvidenceIDs       []string            `json:"evidence_ids"`
	Predicate         EvidencePredicate   `json:"predicate"`
	References        []EvidenceReference `json:"references"`
	Triage            string              `json:"triage"`
	Status            string              `json:"status"`
	SourceStatus      string              `json:"source_status"`
	Review            *EvidenceReview     `json:"review,omitempty"`
	Retests           []FindingRetest     `json:"retests,omitempty"`
	CreatedSequence   int                 `json:"created_sequence"`
	UpdatedAt         time.Time           `json:"updated_at"`
}

type FindingRetest struct {
	EvidenceIDs []string            `json:"evidence_ids"`
	References  []EvidenceReference `json:"references"`
	FindingHash string              `json:"finding_hash"`
	Status      string              `json:"status"`
	Reason      string              `json:"reason"`
	CreatedAt   time.Time           `json:"created_at"`
}
