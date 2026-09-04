## 1. Capture implementation

- [x] 1.1 Remove per-frame chunk capture and the frame budget from `OpenAIClient.StreamChat`
- [x] 1.2 Keep historical chunk kind/frame-index semantics in model and schema comments

## 2. Tests and validation

- [x] 2.1 Update capture tests to assert request/response ordering and absence of chunk rows
- [x] 2.2 Run focused agent capture tests
- [x] 2.3 Run `go vet` for the affected packages
