package pipeline

import (
	"fmt"
	"sort"
	"strings"
)

type ChatContext struct {
	UserID         string
	OriginalPrompt string
	SystemMessage  string
	Blocks         []ContextBlock
}

type ContextBlock struct {
	Source   string
	Tag      string
	Content  string
	Priority int
}

func (ctx *ChatContext) AddBlock(b ContextBlock) {
	ctx.Blocks = append(ctx.Blocks, b)
}

func (ctx *ChatContext) RenderMemory() string {
	if len(ctx.Blocks) == 0 {
		return ""
	}
	sort.SliceStable(ctx.Blocks, func(i, j int) bool {
		return ctx.Blocks[i].Priority < ctx.Blocks[j].Priority
	})

	var b strings.Builder
	b.WriteString("<memory>\n")
	for _, block := range ctx.Blocks {
		fmt.Fprintf(&b, "<%s>\n%s\n</%s>\n", block.Tag, block.Content, block.Tag)
	}
	b.WriteString("</memory>\n")
	return b.String()
}
