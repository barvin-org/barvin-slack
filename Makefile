.PHONY: clean build

all: build

build:
	go build -race -v -x

clean:
	-rm -rf barvin-slack
