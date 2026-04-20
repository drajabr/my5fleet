CC ?= gcc
CFLAGS ?= -O2 -pipe -Wall -Wextra -std=c11
LDFLAGS ?= -lX11
PREFIX ?= /usr/local

all: autotilingwm

autotilingwm: autotilingwm.c
	$(CC) $(CFLAGS) -s -o $@ $< $(LDFLAGS)

install: autotilingwm
	install -Dm755 autotilingwm $(PREFIX)/bin/autotilingwm

clean:
	rm -f autotilingwm

.PHONY: all install clean
