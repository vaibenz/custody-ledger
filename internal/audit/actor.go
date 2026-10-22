package audit

import "context"

type actorKey struct{}

// WithActor records who is acting for the rest of a request, so lower
// layers can audit without every function taking an extra argument.
func WithActor(ctx context.Context, actor string) context.Context {
	return context.WithValue(ctx, actorKey{}, actor)
}

// Actor returns the actor stored in ctx, or "system" if none was set.
func Actor(ctx context.Context) string {
	if a, ok := ctx.Value(actorKey{}).(string); ok && a != "" {
		return a
	}
	return "system"
}
