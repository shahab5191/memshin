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

	// Focus is what the conversation is currently about: the topics a reference
	// like "that" or "the other one" could be pointing at. The focus layer
	// writes it; layers after it read it.
	//
	// It travels as a field rather than as a Block because downstream layers
	// consume it programmatically — mid-term hands it to the decomposer to
	// resolve references before probing — and a rendered string would have to
	// be parsed back apart. The focus layer separately adds its own tagged
	// Block for the model to read.
	//
	// This makes layer order load-bearing for the layers that read it, which
	// nothing else in the pipeline requires. See the ordering note where the
	// engine's layers are assembled.
	Focus []string
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
