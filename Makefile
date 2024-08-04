MAKE_DIR=.make

default: test

tools: .make/tools

generate: .make/tools
	@go generate -v ./...

test: generate
	@go mod tidy
	@go test ./...

clean:
	@rm -rf ${MAKE_DIR}

${MAKE_DIR}:
	@mkdir -p $@

${MAKE_DIR}/tools: ${MAKE_DIR}
	@go install go.uber.org/mock/mockgen@latest
	@touch $@