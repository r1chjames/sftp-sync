LABEL       = com.r1chjames.sftpsyncd
INSTALL_DIR = /usr/local/bin
PLIST_DIR   = $(HOME)/Library/LaunchAgents
PLIST_FILE  = $(PLIST_DIR)/$(LABEL).plist

.PHONY: build install uninstall

build:
	go build -o sftpsyncd ./cmd/sftpsyncd
	go build -o sftpsync  ./cmd/sftpsync

install: build
	sudo install -m 755 sftpsyncd $(INSTALL_DIR)/sftpsyncd
	sudo install -m 755 sftpsync  $(INSTALL_DIR)/sftpsync
	mkdir -p $(PLIST_DIR)
	@printf '<?xml version="1.0" encoding="UTF-8"?>\n\
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"\n\
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">\n\
<plist version="1.0">\n\
<dict>\n\
    <key>Label</key>\n\
    <string>$(LABEL)</string>\n\
    <key>ProgramArguments</key>\n\
    <array>\n\
        <string>$(INSTALL_DIR)/sftpsyncd</string>\n\
    </array>\n\
    <key>RunAtLoad</key>\n\
    <true/>\n\
    <key>KeepAlive</key>\n\
    <true/>\n\
    <key>StandardOutPath</key>\n\
    <string>/tmp/sftpsyncd.log</string>\n\
    <key>StandardErrorPath</key>\n\
    <string>/tmp/sftpsyncd.err</string>\n\
</dict>\n\
</plist>\n' > $(PLIST_FILE)
	launchctl bootstrap gui/$$(id -u) $(PLIST_FILE)
	@echo "sftpsyncd installed and started."

uninstall:
	-launchctl bootout gui/$$(id -u) $(PLIST_FILE) 2>/dev/null
	-sudo rm -f $(INSTALL_DIR)/sftpsyncd $(INSTALL_DIR)/sftpsync
	-rm -f $(PLIST_FILE)
	@echo "sftpsyncd uninstalled."
