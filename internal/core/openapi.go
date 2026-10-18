package core

// OpenAPI is generated directly from the same authorized action descriptors
// consumed by REST, MCP and the UI; provider additions require no second schema.
func (s *Service) OpenAPI(actor Actor) map[string]any {
	paths := map[string]any{}
	for _, action := range s.Actions(actor) {
		successSchema := map[string]any{"type": "object", "properties": map[string]any{"data": map[string]any{}}, "required": []string{"data"}}
		if action.OutputSchema != nil {
			successSchema["properties"] = map[string]any{"data": action.OutputSchema}
		}
		operation := map[string]any{"operationId": action.Name, "summary": action.Description, "x-powerlinux-scope": action.Scope, "x-powerlinux-read-only": action.ReadOnly, "x-powerlinux-task": action.Task, "security": []any{map[string]any{"bearerAuth": []string{}}, map[string]any{"sessionCookie": []string{}}}, "requestBody": map[string]any{"required": true, "content": map[string]any{"application/json": map[string]any{"schema": action.InputSchema}}}, "responses": map[string]any{"200": map[string]any{"description": "Action result or accepted durable task", "content": map[string]any{"application/json": map[string]any{"schema": successSchema}}}, "default": map[string]any{"description": "Structured authorization, input, conflict or execution error", "content": map[string]any{"application/json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/ErrorEnvelope"}}}}}}
		if !action.ReadOnly || action.Task {
			operation["parameters"] = []any{map[string]any{"name": "Idempotency-Key", "in": "header", "required": false, "description": "Stable key for one mutation intent; reuse it on retry", "schema": map[string]any{"type": "string", "minLength": 8, "maxLength": 128, "pattern": validIdempotencyKey.String()}}}
		}
		paths["/api/v1/actions/"+action.Name] = map[string]any{"post": operation}
	}
	return map[string]any{"openapi": "3.1.0", "info": map[string]any{"title": "Folio shared action API", "version": "0.2.0-dev"}, "paths": paths, "components": map[string]any{"securitySchemes": map[string]any{"bearerAuth": map[string]any{"type": "http", "scheme": "bearer"}, "sessionCookie": map[string]any{"type": "apiKey", "in": "cookie", "name": "pl_session", "description": "Cookie writes also require X-CSRF-Token"}}, "schemas": map[string]any{"ErrorEnvelope": map[string]any{"type": "object", "required": []string{"error"}, "properties": map[string]any{"error": map[string]any{"type": "object", "required": []string{"code", "message"}, "properties": map[string]any{"code": str(), "message": str(), "details": map[string]any{}}}}}}}}
}

// StatusSnapshot is for trusted internal background samplers, without inventing
// an administrator actor or changing the host during a metric read.
func (s *Service) StatusSnapshot() SystemStatus { return s.status.get() }
