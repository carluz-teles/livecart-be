// Package domain holds the idea channel's domain entities. Ideas are global
// (not store-scoped): a user proposes an idea, others vote and comment on it.
package domain

import "time"

// Idea is the aggregate root for a feature request in the ideas channel. Fields
// are private; the entity is loaded from persistence via Reconstruct and exposed
// through immutable getters. It carries a few caller-relative projections
// (authorName, votedByMe) that are computed at read time.
type Idea struct {
	id           string
	number       int64
	title        string
	description  string
	category     string
	status       string
	authorID     string
	authorName   string
	voteCount    int
	commentCount int
	votedByMe    bool
	createdAt    time.Time
}

// Reconstruct rebuilds an Idea from persistence data (no validation — the row is
// assumed already-valid). votedByMe/authorName are caller-relative projections
// resolved by the read query.
func Reconstruct(
	id string,
	number int64,
	title string,
	description string,
	category string,
	status string,
	authorID string,
	authorName string,
	voteCount int,
	commentCount int,
	votedByMe bool,
	createdAt time.Time,
) *Idea {
	return &Idea{
		id:           id,
		number:       number,
		title:        title,
		description:  description,
		category:     category,
		status:       status,
		authorID:     authorID,
		authorName:   authorName,
		voteCount:    voteCount,
		commentCount: commentCount,
		votedByMe:    votedByMe,
		createdAt:    createdAt,
	}
}

// Getters (immutable access)

func (i *Idea) ID() string           { return i.id }
func (i *Idea) Number() int64        { return i.number }
func (i *Idea) Title() string        { return i.title }
func (i *Idea) Description() string  { return i.description }
func (i *Idea) Category() string     { return i.category }
func (i *Idea) Status() string       { return i.status }
func (i *Idea) AuthorID() string     { return i.authorID }
func (i *Idea) AuthorName() string   { return i.authorName }
func (i *Idea) VoteCount() int       { return i.voteCount }
func (i *Idea) CommentCount() int    { return i.commentCount }
func (i *Idea) VotedByMe() bool      { return i.votedByMe }
func (i *Idea) CreatedAt() time.Time { return i.createdAt }

// IsAuthoredBy reports whether the given user proposed this idea. Used to block
// self-voting and to flag the caller's own ideas in the feed.
func (i *Idea) IsAuthoredBy(userID string) bool {
	return i.authorID == userID
}

// IdeaDetail is the read model for a single idea plus its threaded comments.
type IdeaDetail struct {
	idea     *Idea
	comments []*Comment
}

// NewIdeaDetail assembles the detail read model.
func NewIdeaDetail(idea *Idea, comments []*Comment) *IdeaDetail {
	return &IdeaDetail{idea: idea, comments: comments}
}

func (d *IdeaDetail) Idea() *Idea          { return d.idea }
func (d *IdeaDetail) Comments() []*Comment { return d.comments }

// VoteResult is the outcome of toggling a vote: the new count and whether the
// caller currently has an active vote.
type VoteResult struct {
	voteCount int
	votedByMe bool
}

// NewVoteResult builds a vote toggle result.
func NewVoteResult(voteCount int, votedByMe bool) *VoteResult {
	return &VoteResult{voteCount: voteCount, votedByMe: votedByMe}
}

func (v *VoteResult) VoteCount() int  { return v.voteCount }
func (v *VoteResult) VotedByMe() bool { return v.votedByMe }
