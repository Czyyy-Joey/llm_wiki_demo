package domain

import (
	"encoding/json"
	"time"
)

type PageType string

const (
	PageTypeConcept PageType = "concept"
	PageTypeEntity  PageType = "entity"
	PageTypeTopic   PageType = "topic"
)

type PageStatus string

const (
	PageStatusActive   PageStatus = "active"
	PageStatusMerged   PageStatus = "merged"
	PageStatusArchived PageStatus = "archived"
)

type ClaimType string

const (
	ClaimFact       ClaimType = "fact"
	ClaimDefinition ClaimType = "definition"
	ClaimArgument   ClaimType = "argument"
	ClaimProcedure  ClaimType = "procedure"
	ClaimCaveat     ClaimType = "caveat"
)

type PlanAction string

const (
	ActionCreate PlanAction = "CREATE"
	ActionUpdate PlanAction = "UPDATE"
	ActionMerge  PlanAction = "MERGE"
	ActionLink   PlanAction = "LINK"
	ActionNoOp   PlanAction = "NO_OP"
)

type SourceDocument struct {
	ID            string    `json:"id"`
	OriginalName  string    `json:"original_name"`
	MediaType     string    `json:"media_type"`
	SHA256        string    `json:"sha256"`
	OriginalPath  string    `json:"original_path"`
	ParsedPath    string    `json:"parsed_path"`
	Status        string    `json:"status"`
	ParserVersion string    `json:"parser_version"`
	CreatedAt     time.Time `json:"created_at"`
	ParseError    string    `json:"parse_error,omitempty"`
}
type SourceChunk struct {
	ID          string   `json:"id"`
	DocumentID  string   `json:"document_id"`
	ChunkIndex  int      `json:"chunk_index"`
	Text        string   `json:"text"`
	PageNumber  *int     `json:"page_number,omitempty"`
	HeadingPath []string `json:"heading_path,omitempty"`
	CharStart   int      `json:"char_start"`
	CharEnd     int      `json:"char_end"`
	ContentHash string   `json:"content_hash"`
}
type WikiPage struct {
	ID              string     `json:"id"`
	Slug            string     `json:"slug"`
	PageType        PageType   `json:"page_type"`
	Title           string     `json:"title"`
	Summary         string     `json:"summary"`
	Status          PageStatus `json:"status"`
	CurrentRevision int        `json:"current_revision"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}
type WikiSection struct {
	ID       string `json:"id"`
	PageID   string `json:"page_id"`
	Heading  string `json:"heading"`
	Position int    `json:"position"`
	Summary  string `json:"summary"`
}
type WikiClaim struct {
	ID             string    `json:"id"`
	PageID         string    `json:"page_id"`
	SectionID      string    `json:"section_id,omitempty"`
	Text           string    `json:"text"`
	ClaimType      ClaimType `json:"claim_type"`
	Status         string    `json:"status"`
	CreatedByRunID string    `json:"created_by_run_id"`
	UpdatedByRunID string    `json:"updated_by_run_id"`
}
type ClaimEvidence struct {
	ClaimID       string `json:"claim_id"`
	SourceChunkID string `json:"source_chunk_id"`
	Relation      string `json:"relation"`
	Note          string `json:"note,omitempty"`
}
type WikiLink struct {
	SourcePageID   string `json:"source_page_id"`
	TargetPageID   string `json:"target_page_id"`
	Relation       string `json:"relation"`
	CreatedByRunID string `json:"created_by_run_id"`
}
type WikiRevision struct {
	PageID           string          `json:"page_id"`
	RevisionNumber   int             `json:"revision_number"`
	CompilationRunID string          `json:"compilation_run_id"`
	Snapshot         json.RawMessage `json:"snapshot"`
	ChangeSummary    string          `json:"change_summary"`
	CreatedAt        time.Time       `json:"created_at"`
}
type CompilationPlan struct {
	DocumentID    string       `json:"document_id"`
	PageActions   []PageAction `json:"page_actions"`
	SchemaVersion string       `json:"schema_version"`
}
type PageAction struct {
	Action       PlanAction    `json:"action"`
	TargetPageID string        `json:"target_page_id,omitempty"`
	SourcePageID string        `json:"source_page_id,omitempty"`
	Relation     string        `json:"relation,omitempty"`
	Slug         string        `json:"slug,omitempty"`
	PageType     PageType      `json:"page_type,omitempty"`
	Title        string        `json:"title,omitempty"`
	Summary      string        `json:"summary,omitempty"`
	Reason       string        `json:"reason"`
	ClaimActions []ClaimAction `json:"claim_actions,omitempty"`
}
type ClaimAction struct {
	Action           string    `json:"action"`
	Text             string    `json:"text"`
	ClaimType        ClaimType `json:"claim_type,omitempty"`
	TargetClaimID    string    `json:"target_claim_id,omitempty"`
	PreviousText     string    `json:"previous_text,omitempty"`
	EvidenceChunkIDs []string  `json:"evidence_chunk_ids"`
}
type SourceAnalysis struct {
	DocumentID string             `json:"document_id"`
	Summary    string             `json:"summary"`
	Topics     []AnalyzedTopic    `json:"topics"`
	Relations  []AnalyzedRelation `json:"relations"`
}
type AnalyzedRelation struct {
	SourceTopicKey string `json:"source_topic_key"`
	TargetTopicKey string `json:"target_topic_key"`
	Relation       string `json:"relation"`
}
type AnalyzedTopic struct {
	Key      string          `json:"key"`
	Title    string          `json:"title"`
	Slug     string          `json:"slug"`
	PageType PageType        `json:"page_type"`
	Summary  string          `json:"summary"`
	Claims   []AnalyzedClaim `json:"claims"`
}
type AnalyzedClaim struct {
	Text             string    `json:"text"`
	ClaimType        ClaimType `json:"claim_type"`
	EvidenceChunkIDs []string  `json:"evidence_chunk_ids"`
}
type CompilationCandidate struct {
	TopicKey   string   `json:"topic_key"`
	PageID     string   `json:"page_id"`
	Slug       string   `json:"slug"`
	Title      string   `json:"title"`
	PageType   PageType `json:"page_type"`
	Summary    string   `json:"summary"`
	Score      float64  `json:"score"`
	ClaimTexts []string `json:"claim_texts"`
}
type RetrievalTrace struct {
	ID               string             `json:"id"`
	NormalizedQuery  string             `json:"normalized_query"`
	FTSCandidates    []string           `json:"fts_candidates"`
	VectorCandidates []string           `json:"vector_candidates"`
	RRFScore         map[string]float64 `json:"rrf_score"`
	ExpandedPages    []string           `json:"expanded_pages"`
}
type Conversation struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
type Message struct {
	ID               string    `json:"id"`
	ConversationID   string    `json:"conversation_id"`
	Role             string    `json:"role"`
	Content          string    `json:"content"`
	RetrievalTraceID string    `json:"retrieval_trace_id,omitempty"`
	Citations        []string  `json:"citations,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
}
