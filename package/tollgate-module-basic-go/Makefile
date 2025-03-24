include $(TOPDIR)/rules.mk

PKG_NAME:=tollgate-module-basic-go
PKG_VERSION:=$(shell git rev-list --count HEAD 2>/dev/null || echo "0.0.1").$(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
PKG_RELEASE:=1

# Place conditional checks EARLY - before variables that depend on them
ifneq ($(TOPDIR),)
	# Feed-specific settings (auto-clone from git)
	PKG_SOURCE_PROTO:=git
	PKG_SOURCE_URL:=https://github.com/OpenTollGate/tollgate-module-basic-go.git
	PKG_SOURCE_VERSION:=$(shell git rev-parse HEAD) # Use exact current commit
	PKG_MIRROR_HASH:=skip
else
	# SDK build context (local files)
	PKG_BUILD_DIR:=$(CURDIR)
endif

PKG_MAINTAINER:=Your Name <your@email.com>
PKG_LICENSE:=CC0-1.0
PKG_LICENSE_FILES:=LICENSE

PKG_BUILD_DEPENDS:=golang/host
PKG_BUILD_PARALLEL:=1
PKG_USE_MIPS16:=0

GO_PKG:=github.com/OpenTollGate/tollgate-module-basic-go

include $(INCLUDE_DIR)/package.mk
# include $(INCLUDE_DIR)/golang-package.mk
$(eval $(call GoPackage))

define Package/$(PKG_NAME)
	SECTION:=net
	CATEGORY:=Network
	TITLE:=TollGate Basic Module
	DEPENDS:=$(GO_ARCH_DEPENDS)
endef

define Package/$(PKG_NAME)/description
	TollGate Basic Module for OpenWrt
endef

define Build/Prepare
	$(call Build/Prepare/Default)
	echo "DEBUG: Contents of go.mod after prepare:"
	cat $(PKG_BUILD_DIR)/go.mod
endef

define Build/Configure
endef

# define Build/Compile
# 	mkdir -p "$(PKG_BUILD_DIR)/.go_work/build/src/github.com/OpenTollGate"
# 	ln -sf "$(PKG_BUILD_DIR)" "$(PKG_BUILD_DIR)/.go_work/build/src/github.com/OpenTollGate/tollgate-module-basic-go"

# 	# Simplify the Go build process to match the working command
# 	cd $(PKG_BUILD_DIR) && \
# 	GOOS=linux \
# 	GOARCH=arm64 \
# 	go build -o tollgate-basic -trimpath -ldflags="-s -w"

# 	# Verify the binary type
# 	file $(PKG_BUILD_DIR)/tollgate-basic

# 	@if [ ! -f "$(PKG_BUILD_DIR)/tollgate-basic" ]; then \
# 		echo "\n!!! NO BINARY CREATED - BUILD FAILED !!!"; \
# 		echo "Build artifacts:" ; \
# 		ls -la $(PKG_BUILD_DIR) ; \
# 		exit 1; \
# 	fi

# 	# Create symlink to binary in expected location for install phase
# 	mkdir -p $(PKG_BUILD_DIR)/.go_work/build/bin
# 	ln -sf $(PKG_BUILD_DIR)/tollgate-basic $(PKG_BUILD_DIR)/.go_work/build/bin/tollgate-basic
# endef

define Build/Compile
	cd $(PKG_BUILD_DIR) && \
	GOOS=linux GOARCH=arm64 \
	go build -o $(PKG_NAME) -trimpath -ldflags="-s -w" 
endef

define Package/$(PKG_NAME)/install
	$(INSTALL_DIR) $(1)/usr/bin
	$(INSTALL_BIN) $(PKG_BUILD_DIR)/$(PKG_NAME) $(1)/usr/bin/tollgate-basic
endef

$(eval $(call BuildPackage,$(PKG_NAME)))

# Print IPK path after successful compilation
PKG_FINISH:=$(shell echo "Successfully built: $(IPK_FILE)" >&2)
