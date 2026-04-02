.PHONY: all build run stop

all: run

stop:
	@if lsof -ti tcp:8080 >/dev/null 2>&1; then \
		kill $$(lsof -ti tcp:8080); \
	fi

build:
	go build -o kindlevibe .

run: stop build
	./kindlevibe start
