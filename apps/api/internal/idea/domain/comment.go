package domain

import "time"

// Comment is a node in an idea's threaded discussion. Replies are held as child
// Comments so the presentation layer can render the tree directly from the
// entity. Fields are private; loaded via Reconstruct.
type Comment struct {
	id              string
	ideaID          string
	parentCommentID *string
	authorID        string
	authorName      string
	body            string
	createdAt       time.Time
	replies         []*Comment
}

// ReconstructComment rebuilds a Comment from persistence data. replies start
// empty and are linked by BuildTree.
func ReconstructComment(
	id string,
	ideaID string,
	parentCommentID *string,
	authorID string,
	authorName string,
	body string,
	createdAt time.Time,
) *Comment {
	return &Comment{
		id:              id,
		ideaID:          ideaID,
		parentCommentID: parentCommentID,
		authorID:        authorID,
		authorName:      authorName,
		body:            body,
		createdAt:       createdAt,
		replies:         []*Comment{},
	}
}

// Getters (immutable access)

func (c *Comment) ID() string               { return c.id }
func (c *Comment) IdeaID() string           { return c.ideaID }
func (c *Comment) ParentCommentID() *string { return c.parentCommentID }
func (c *Comment) AuthorID() string         { return c.authorID }
func (c *Comment) AuthorName() string       { return c.authorName }
func (c *Comment) Body() string             { return c.body }
func (c *Comment) CreatedAt() time.Time     { return c.createdAt }
func (c *Comment) Replies() []*Comment      { return c.replies }

// IsReply reports whether this comment is a reply to another comment.
func (c *Comment) IsReply() bool { return c.parentCommentID != nil }

// BuildTree assembles a parent→replies forest from a flat, time-ordered comment
// list. Single O(n) pass: index every node by id, then attach each reply to its
// parent. Orphans (missing parent) are promoted to roots so nothing is dropped.
func BuildTree(comments []*Comment) []*Comment {
	byID := make(map[string]*Comment, len(comments))
	for _, c := range comments {
		c.replies = []*Comment{}
		byID[c.id] = c
	}

	roots := make([]*Comment, 0, len(comments))
	for _, c := range comments {
		if c.parentCommentID == nil {
			roots = append(roots, c)
			continue
		}
		parent, ok := byID[*c.parentCommentID]
		if !ok {
			roots = append(roots, c)
			continue
		}
		parent.replies = append(parent.replies, c)
	}
	return roots
}
