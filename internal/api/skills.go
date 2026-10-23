package api

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// `qube skills install`'s one route: QuBe Sync serves each agent skill it publishes at
// /skills/<name>/SKILL.md, as Markdown, to anyone. newRequest sends no token there, so it
// works before a login and never carries an act-as grant.

// MaxSkillBytes is the largest SKILL.md Skill takes: a bigger answer is refused, not cut.
const MaxSkillBytes = 1 << 20

// SkillPath is where the host serves the skill name (a name SkillName accepts).
func SkillPath(name string) string {
	return "/skills/" + url.PathEscape(name) + "/SKILL.md"
}

// SkillTooLarge is a SKILL.md over MaxSkillBytes.
type SkillTooLarge struct{ Path string }

func (e *SkillTooLarge) Error() string {
	return fmt.Sprintf("GET %s answered more than 1 MB, which no skill is; refusing it", e.Path)
}

// Skill is the SKILL.md the host serves for name. Any answer but a 200 is an *Error with its
// status (a 404: the host has no skill by that name).
func (c *Client) Skill(name string) ([]byte, error) {
	path := SkillPath(name)
	req, err := c.newRequest("GET", path, nil, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/markdown, text/plain;q=0.9, */*;q=0.1")
	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, unwrapCanceled(err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, MaxSkillBytes+1))
	if err != nil {
		return nil, unwrapCanceled(err)
	}
	if res.StatusCode != http.StatusOK {
		return nil, &Error{Status: res.StatusCode, Body: string(data)}
	}
	if len(data) > MaxSkillBytes {
		return nil, &SkillTooLarge{Path: path}
	}
	return data, nil
}
