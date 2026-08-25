/*
 * Brclio Mail UI
 * Derived from ESTHER不二 / esthersjw Esther Design System.
 * https://github.com/esthersjw/esther-design-system · CC BY-NC-SA 4.0
 */

(() => {
  "use strict";

  const API_BASE = "/api";
  const REQUEST_TIMEOUT = 20_000;
  const COMPOSE_FILE_LIMIT = 10 * 1024 * 1024;
  const MAILBOX_LABELS = Object.freeze({
    INBOX: "收件箱",
    Starred: "已加星",
    Drafts: "草稿",
    Sent: "已发送",
    Archive: "归档",
    Junk: "垃圾邮件",
    Trash: "垃圾箱",
  });

  const ADMIN_SECTIONS = Object.freeze({
    overview: {
      title: "邮局概览",
      description: "域名、账号、归档空间与投递状态都在这里。",
      endpoint: "/admin/stats",
    },
    domains: {
      title: "域名与 DNS",
      description: "添加邮件域名，并逐项核对所有权、MX、SPF、DKIM 与 DMARC。",
      endpoint: "/admin/domains",
      action: "添加域名",
    },
    users: {
      title: "邮箱账号",
      description: "分发邮箱、控制状态，并为每位用户设置独立空间。",
      endpoint: "/admin/users",
      action: "新建邮箱",
    },
    aliases: {
      title: "邮箱别名",
      description: "别名只负责投递，不可作为独立账号登录。",
      endpoint: "/admin/aliases",
      action: "添加别名",
    },
    archive: {
      title: "留存归档",
      description:
        "查看经系统收发并留存的邮件。每次阅读都会记录理由和管理员身份。",
      endpoint: "/admin/archive",
    },
    queue: {
      title: "投递队列",
      description: "查看排队、重试、成功与失败的外发投递。",
      endpoint: "/admin/queue",
    },
    audit: {
      title: "审计记录",
      description: "追踪管理员操作、归档访问和账号安全事件。",
      endpoint: "/admin/audit",
    },
  });

  const state = {
    status: null,
    me: null,
    clientConfig: null,
    mode: "mail",
    mailbox: "INBOX",
    query: "",
    page: 1,
    messages: [],
    messageMeta: {},
    selectedMessage: null,
    mailboxes: [],
    messageRequest: 0,
    adminSection: "overview",
    adminPages: { archive: 1, audit: 1 },
    adminData: null,
    adminRequest: 0,
    archiveTarget: null,
    archiveReason: "",
    composeFiles: [],
    draftId: null,
    draftTimer: null,
    draftSavePromise: Promise.resolve(),
    composeGeneration: 0,
    appPasswords: [],
    confirmResolver: null,
    authRedirecting: false,
  };

  const ids = [
    "boot-screen",
    "connection-view",
    "connection-message",
    "retry-connection",
    "setup-view",
    "setup-form",
    "setup-error",
    "setup-password",
    "setup-confirm",
    "login-view",
    "login-form",
    "login-error",
    "login-email",
    "app-shell",
    "sidebar-toggle",
    "sidebar-scrim",
    "primary-sidebar",
    "brand-home",
    "main-content",
    "global-search",
    "search-input",
    "mode-switch",
    "compose-trigger",
    "account-trigger",
    "avatar-initials",
    "header-user-name",
    "mail-navigation",
    "admin-navigation",
    "quota-card",
    "quota-label",
    "quota-progress",
    "quota-caption",
    "mail-view",
    "message-list-panel",
    "mailbox-kicker",
    "mailbox-title",
    "refresh-messages",
    "message-list-status",
    "message-list",
    "message-pagination",
    "previous-page",
    "next-page",
    "page-label",
    "message-reader",
    "reader-empty",
    "reader-loading",
    "reader-content",
    "reader-back",
    "reader-label",
    "reader-subject",
    "message-star",
    "message-reply",
    "message-delete",
    "sender-avatar",
    "reader-from",
    "reader-date",
    "reader-details-toggle",
    "reader-details",
    "reader-attachments",
    "reader-body",
    "admin-view",
    "admin-kicker",
    "admin-title",
    "admin-description",
    "admin-primary-action",
    "admin-content",
    "compose-dialog",
    "compose-form",
    "compose-to",
    "compose-cc",
    "compose-bcc",
    "compose-subject",
    "compose-body",
    "toggle-copy-fields",
    "copy-fields",
    "compose-files",
    "compose-attachments",
    "compose-error",
    "draft-status",
    "account-dialog",
    "account-summary",
    "new-app-password",
    "app-password-list",
    "app-password-form",
    "app-password-name",
    "app-password-secret",
    "protocol-imap",
    "protocol-smtp",
    "protocol-username",
    "logout-button",
    "admin-form-dialog",
    "admin-form",
    "admin-form-title",
    "admin-form-kicker",
    "admin-form-fields",
    "admin-form-error",
    "archive-reason-dialog",
    "archive-reason-form",
    "archive-reason",
    "archive-reason-error",
    "archive-message-dialog",
    "archive-message-title",
    "archive-message-content",
    "confirm-dialog",
    "confirm-form",
    "confirm-title",
    "confirm-description",
    "confirm-accept",
    "toast-region",
    "app-live-region",
  ];

  const el = Object.fromEntries(
    ids.map((id) => [camel(id), document.getElementById(id)]),
  );

  class APIError extends Error {
    constructor(message, status = 0, payload = null) {
      super(message);
      this.name = "APIError";
      this.status = status;
      this.payload = payload;
    }
  }

  document.addEventListener("DOMContentLoaded", init);

  function camel(value) {
    return value.replace(/-([a-z])/g, (_, letter) => letter.toUpperCase());
  }

  async function init() {
    bindEvents();
    await bootstrap();
  }

  function bindEvents() {
    el.retryConnection.addEventListener("click", bootstrap);
    el.setupForm.addEventListener("submit", handleSetup);
    el.loginForm.addEventListener("submit", handleLogin);
    el.sidebarToggle.addEventListener("click", toggleSidebar);
    el.sidebarScrim.addEventListener("click", closeSidebar);
    el.brandHome.addEventListener("click", handleBrandHome);
    el.globalSearch.addEventListener("submit", handleSearch);
    el.searchInput.addEventListener("search", handleSearch);
    el.composeTrigger.addEventListener("click", () => openCompose());
    el.accountTrigger.addEventListener("click", openAccount);
    el.refreshMessages.addEventListener("click", () =>
      loadMessages({ preserveSelection: true }),
    );
    el.previousPage.addEventListener("click", () => changePage(-1));
    el.nextPage.addEventListener("click", () => changePage(1));
    el.messageList.addEventListener("click", handleMessageListClick);
    el.messageList.addEventListener("keydown", handleMessageListKeydown);
    el.readerBack.addEventListener("click", closeReaderOnMobile);
    el.readerDetailsToggle.addEventListener("click", toggleReaderDetails);
    el.messageStar.addEventListener("click", toggleSelectedStar);
    el.messageReply.addEventListener("click", replyToSelected);
    el.messageDelete.addEventListener("click", deleteSelectedMessage);
    el.mailNavigation.addEventListener("click", handleMailboxNavigation);
    el.adminNavigation.addEventListener("click", handleAdminNavigation);
    el.modeSwitch.addEventListener("click", handleModeSwitch);
    el.adminPrimaryAction.addEventListener("click", openAdminCreateDialog);
    el.adminContent.addEventListener("click", handleAdminContentClick);
    el.adminContent.addEventListener("input", handleAdminFilter);
    el.composeForm.addEventListener("submit", sendMessage);
    el.composeForm.addEventListener("input", scheduleDraftSave);
    el.composeDialog.addEventListener("cancel", handleComposeCancel);
    el.toggleCopyFields.addEventListener("click", toggleCopyFields);
    el.composeFiles.addEventListener("change", addComposeFiles);
    el.composeAttachments.addEventListener("click", removeComposeFile);
    el.newAppPassword.addEventListener("click", toggleAppPasswordForm);
    el.appPasswordForm.addEventListener("submit", createAppPassword);
    el.appPasswordList.addEventListener("click", revokeAppPassword);
    el.accountDialog.addEventListener("close", clearAppPasswordSecret);
    el.logoutButton.addEventListener("click", logout);
    el.adminForm.addEventListener("submit", submitAdminForm);
    el.archiveReasonForm.addEventListener("submit", viewArchivedMessage);
    el.archiveMessageContent.addEventListener(
      "click",
      downloadArchivedAttachment,
    );
    el.confirmForm.addEventListener("submit", resolveConfirmTrue);
    el.confirmDialog.addEventListener("cancel", resolveConfirmFalse);
    document.addEventListener("click", handleDocumentClick);
    document.addEventListener("keydown", handleGlobalKeydown);
    window.addEventListener("resize", handleResize);

    document.querySelectorAll("[data-mobile-action]").forEach((button) => {
      button.addEventListener("click", handleMobileAction);
    });
  }

  async function bootstrap() {
    showTopLevel("boot");
    state.authRedirecting = false;
    try {
      state.status = await request("/status", { suppressAuthRedirect: true });
      if (requiresSetup(state.status)) {
        showTopLevel("setup");
        return;
      }
      try {
        const payload = await request("/me", { suppressAuthRedirect: true });
        applyMePayload(payload);
        await enterApp();
      } catch (error) {
        if (
          error instanceof APIError &&
          (error.status === 401 || error.status === 403)
        ) {
          showTopLevel("login");
          return;
        }
        throw error;
      }
    } catch (error) {
      el.connectionMessage.textContent = humanError(
        error,
        "无法连接邮件服务。请检查服务状态后重试。",
      );
      showTopLevel("connection");
    }
  }

  function requiresSetup(payload) {
    const data = normalizeEntity(payload, ["status"]);
    if (typeof data.setupRequired === "boolean") return data.setupRequired;
    if (typeof data.needsSetup === "boolean") return data.needsSetup;
    if (typeof data.initialized === "boolean") return !data.initialized;
    if (typeof data.configured === "boolean") return !data.configured;
    const status = String(data.status || data.state || "").toLowerCase();
    return [
      "setup",
      "setup_required",
      "uninitialized",
      "not_configured",
    ].includes(status);
  }

  function showTopLevel(view) {
    const map = {
      boot: el.bootScreen,
      connection: el.connectionView,
      setup: el.setupView,
      login: el.loginView,
      app: el.appShell,
    };
    Object.values(map).forEach((node) => {
      node.hidden = true;
    });
    map[view].hidden = false;
    if (view === "login") requestAnimationFrame(() => el.loginEmail.focus());
  }

  async function handleSetup(event) {
    event.preventDefault();
    clearFormError(el.setupError, el.setupForm);
    const data = new FormData(el.setupForm);
    const password = String(data.get("password") || "");
    const confirmPassword = String(data.get("confirmPassword") || "");
    if (!el.setupForm.reportValidity()) return;
    if (password !== confirmPassword) {
      showFormError(el.setupError, "两次输入的密码不一致。", el.setupConfirm);
      return;
    }

    setFormBusy(el.setupForm, true, "正在创建…");
    try {
      await request("/setup", {
        method: "POST",
        body: {
          setupToken: String(data.get("setupToken") || "").trim(),
          domain: String(data.get("domain") || "").trim(),
          displayName: String(data.get("displayName") || "").trim(),
          email: String(data.get("email") || "").trim(),
          password,
        },
        suppressAuthRedirect: true,
      });
      const mePayload = await request("/me", { suppressAuthRedirect: true });
      applyMePayload(mePayload);
      toast("初始化完成，欢迎接管邮局。", "success");
      await enterApp();
    } catch (error) {
      if (error instanceof APIError && error.status === 401) {
        el.loginEmail.value = String(data.get("email") || "");
        showTopLevel("login");
        toast("管理员已创建，请登录继续。", "success");
      } else {
        showFormError(
          el.setupError,
          humanError(error, "初始化失败，请检查信息后重试。"),
        );
      }
    } finally {
      setFormBusy(el.setupForm, false);
    }
  }

  async function handleLogin(event) {
    event.preventDefault();
    clearFormError(el.loginError, el.loginForm);
    if (!el.loginForm.reportValidity()) return;
    const data = new FormData(el.loginForm);
    setFormBusy(el.loginForm, true, "正在开门…");
    try {
      await request("/auth/login", {
        method: "POST",
        body: {
          email: String(data.get("email") || "").trim(),
          password: String(data.get("password") || ""),
        },
        suppressAuthRedirect: true,
      });
      const mePayload = await request("/me", { suppressAuthRedirect: true });
      applyMePayload(mePayload);
      el.loginForm.reset();
      await enterApp();
    } catch (error) {
      showFormError(
        el.loginError,
        humanError(error, "登录失败，请检查邮箱和密码。"),
      );
    } finally {
      setFormBusy(el.loginForm, false);
    }
  }

  async function enterApp() {
    if (!state.me || !state.me.email) {
      throw new APIError("登录信息不完整，请重新登录。", 401);
    }
    updateAccountUI();
    showTopLevel("app");
    setMode("mail", { load: false });
    state.mailbox = "INBOX";
    state.page = 1;
    state.query = "";
    el.searchInput.value = "";
    el.mailboxKicker.textContent = "LATEST DELIVERY";
    el.mailboxTitle.textContent = mailboxLabel("INBOX");
    el.mailNavigation.querySelectorAll("[data-mailbox]").forEach((button) => {
      button.classList.toggle("active", button.dataset.mailbox === "INBOX");
    });
    clearReader();
    await Promise.allSettled([loadMailboxes(), loadMessages()]);
    el.mainContent?.focus?.({ preventScroll: true });
  }

  function applyMePayload(payload) {
    state.me = normalizeEntity(payload, ["user", "me", "account"]);
    state.clientConfig =
      payload?.clientConfig || payload?.data?.clientConfig || null;
  }

  function updateAccountUI() {
    const displayName = state.me.displayName || state.me.name || state.me.email;
    const initials = getInitials(displayName);
    el.avatarInitials.textContent = initials;
    el.headerUserName.textContent = displayName;
    el.accountTrigger.setAttribute(
      "aria-label",
      `${displayName}，账号与客户端设置`,
    );
    el.protocolUsername.textContent = state.me.email || "—";
    const hostname =
      state.clientConfig?.hostname ||
      state.status?.hostname ||
      window.location.hostname;
    const imapPort = state.clientConfig?.imapPort || 993;
    const smtpPort = state.clientConfig?.smtpPort || 587;
    el.protocolImap.textContent = `${hostname} · ${imapPort} · TLS`;
    el.protocolSmtp.textContent = `${hostname} · ${smtpPort} · STARTTLS`;
    const isAdmin = String(state.me.role || "").toLowerCase() === "admin";
    el.modeSwitch.hidden = !isAdmin;
    const mobileAdmin = document.querySelector('[data-mobile-action="admin"]');
    mobileAdmin.hidden = !isAdmin;
    document
      .querySelector(".mobile-navigation")
      ?.classList.toggle("has-admin", isAdmin);
    renderQuota();
  }

  async function loadMailboxes() {
    try {
      const payload = await request("/mailboxes");
      state.mailboxes = normalizeList(payload, [
        "mailboxes",
        "items",
        "folders",
      ]);
      renderFolderCounts();
    } catch (error) {
      toast(humanError(error, "无法读取文件夹数量。"), "error");
    }
  }

  function renderFolderCounts() {
    const summaries = new Map(
      state.mailboxes.map((item) => [
        String(item.name || item.mailbox || item.id),
        item,
      ]),
    );
    document.querySelectorAll("[data-count]").forEach((node) => {
      const summary = summaries.get(node.dataset.count);
      const count = Number(
        summary?.unread ?? summary?.count ?? summary?.total ?? 0,
      );
      node.textContent =
        count > 0 ? (count > 999 ? "999+" : String(count)) : "";
      node.hidden = count <= 0;
    });
  }

  function renderQuota() {
    if (!state.me) return;
    const used = Number(state.me.usedBytes ?? state.me.storageUsed ?? 0);
    const quota = Number(state.me.quotaBytes ?? state.me.storageQuota ?? 0);
    const ratio =
      quota > 0 ? Math.min(100, Math.max(0, (used / quota) * 100)) : 0;
    el.quotaLabel.textContent = quota > 0 ? `${Math.round(ratio)}%` : "未限制";
    el.quotaProgress.value = ratio;
    el.quotaProgress.textContent = `${Math.round(ratio)}%`;
    el.quotaProgress.classList.toggle("warning", ratio >= 75 && ratio < 90);
    el.quotaProgress.classList.toggle("danger", ratio >= 90);
    el.quotaCaption.textContent =
      quota > 0
        ? `${formatBytes(used)} / ${formatBytes(quota)}`
        : `${formatBytes(used)} 已使用`;
  }

  async function loadMessages({ preserveSelection = false } = {}) {
    const requestNumber = ++state.messageRequest;
    const previousId = preserveSelection
      ? entityID(state.selectedMessage)
      : null;
    el.messageListStatus.className = "panel-status";
    el.messageListStatus.textContent = state.query
      ? `正在搜索“${state.query}”…`
      : "正在收取信件…";
    renderListLoading();

    const params = new URLSearchParams({
      mailbox: state.mailbox,
      page: String(state.page),
    });
    if (state.query) params.set("q", state.query);

    try {
      const payload = await request(`/messages?${params.toString()}`);
      if (requestNumber !== state.messageRequest) return;
      state.messages = normalizeList(payload, ["messages", "items", "results"]);
      state.messageMeta = extractMeta(payload);
      el.messageListStatus.textContent = state.query
        ? `“${state.query}”找到 ${state.messageMeta.total ?? state.messages.length} 封邮件`
        : "";
      renderMessages();
      renderPagination();

      if (
        previousId &&
        state.messages.some((message) => entityID(message) === previousId)
      ) {
        await openMessage(previousId, { focusReader: false });
      } else if (!preserveSelection) {
        clearReader();
      }
    } catch (error) {
      if (requestNumber !== state.messageRequest) return;
      el.messageListStatus.className = "panel-status error";
      el.messageListStatus.textContent = humanError(error, "邮件读取失败。");
      el.messageList.innerHTML = `<div class="list-state"><p>信件暂时没有送到列表。</p><button class="button button-quiet button-small" type="button" data-retry-messages>重试</button></div>`;
      el.messagePagination.hidden = true;
    }
  }

  function renderListLoading() {
    el.messageList.innerHTML = `<div class="list-state" role="status"><span class="spinner" aria-hidden="true"></span><p>正在整理信件…</p></div>`;
    el.messagePagination.hidden = true;
  }

  function renderMessages() {
    if (!state.messages.length) {
      const searchCopy = state.query
        ? "换个关键词再找找"
        : `${mailboxLabel(state.mailbox)}里还没有信件`;
      el.messageList.innerHTML = `
        <div class="list-empty">
          <img src="/assets/private-post-office.png" alt="">
          <strong>${escapeHTML(searchCopy)}</strong>
          <span>${state.query ? "可以搜索发件人、主题或正文。" : "新邮件到达后，会安静地出现在这里。"}</span>
        </div>`;
      return;
    }

    const selectedID = entityID(state.selectedMessage);
    el.messageList.innerHTML = state.messages
      .map((message, index) => {
        const id = entityID(message);
        const unread = !isSeen(message);
        const sender = messageListSender(message);
        const subject = message.subject || "（无主题）";
        const snippet = message.snippet || message.textBody || "";
        const selected = id === selectedID;
        const starred = isStarred(message);
        const attached = Number(message.attachmentCount || 0) > 0;
        return `
        <button class="message-row${unread ? " unread" : ""}" type="button" role="option"
          data-message-id="${escapeHTML(id)}" data-message-index="${index}"
          aria-selected="${selected}">
          <span class="sr-only">${unread ? "未读邮件" : "已读邮件"}</span>
          <span class="message-avatar" aria-hidden="true">${escapeHTML(getInitials(sender))}</span>
          <span class="message-sender">${escapeHTML(sender)}</span>
          <time class="message-time" datetime="${escapeHTML(messageDateISO(message))}">${escapeHTML(formatMessageDate(messageDate(message)))}</time>
          <span class="message-subject">${escapeHTML(subject)}</span>
          <span class="message-snippet">${escapeHTML(snippet)}</span>
          ${starred || attached ? `<span class="row-badges" aria-hidden="true">${starred ? starIcon() : ""}${attached ? paperclipIcon() : ""}</span>` : ""}
        </button>`;
      })
      .join("");
  }

  function renderPagination() {
    const page = Number(state.messageMeta.page || state.page || 1);
    const pages = Number(
      state.messageMeta.pages || state.messageMeta.totalPages || 0,
    );
    const total = Number(
      state.messageMeta.total ?? state.messageMeta.count ?? 0,
    );
    const pageSize = Number(
      state.messageMeta.pageSize || state.messageMeta.limit || 0,
    );
    const hasPrevious = page > 1;
    const explicitHasNext =
      state.messageMeta.hasNext ?? state.messageMeta.hasMore;
    const hasNext =
      typeof explicitHasNext === "boolean"
        ? explicitHasNext
        : pages > 0
          ? page < pages
          : pageSize > 0
            ? state.messages.length >= pageSize && page * pageSize < total
            : false;
    el.previousPage.disabled = !hasPrevious;
    el.nextPage.disabled = !hasNext;
    el.pageLabel.textContent =
      pages > 0 ? `第 ${page} / ${pages} 页` : `第 ${page} 页`;
    el.messagePagination.hidden = !hasPrevious && !hasNext;
  }

  function changePage(delta) {
    state.page = Math.max(1, state.page + delta);
    loadMessages();
  }

  function handleMessageListClick(event) {
    const retry = event.target.closest("[data-retry-messages]");
    if (retry) {
      loadMessages();
      return;
    }
    const row = event.target.closest("[data-message-id]");
    if (row) openMessage(row.dataset.messageId);
  }

  function handleMessageListKeydown(event) {
    const rows = [...el.messageList.querySelectorAll("[data-message-id]")];
    if (!rows.length) return;
    const current = event.target.closest("[data-message-id]");
    const index = current ? rows.indexOf(current) : -1;
    let nextIndex = index;
    if (["ArrowDown", "j", "J"].includes(event.key))
      nextIndex = Math.min(rows.length - 1, index + 1);
    if (["ArrowUp", "k", "K"].includes(event.key))
      nextIndex = Math.max(0, index < 0 ? 0 : index - 1);
    if (nextIndex !== index || index < 0) {
      event.preventDefault();
      rows[nextIndex].focus();
      openMessage(rows[nextIndex].dataset.messageId, { focusReader: false });
    }
  }

  async function openMessage(id, { focusReader = true } = {}) {
    const summary = state.messages.find(
      (message) => entityID(message) === String(id),
    );
    if (summary) state.selectedMessage = summary;
    renderSelectedRow();
    el.readerEmpty.hidden = true;
    el.readerContent.hidden = true;
    el.readerLoading.hidden = false;
    el.mailView.classList.add("reader-open");
    if (focusReader && window.innerWidth <= 900) el.messageReader.focus?.();

    try {
      const payload = await request(`/messages/${encodeURIComponent(id)}`);
      const message = normalizeMessagePayload(payload);
      state.selectedMessage = { ...(summary || {}), ...message };
      renderMessageReader(state.selectedMessage);
      updateMessageSummary(state.selectedMessage);
      if (!isSeen(state.selectedMessage))
        markMessageSeen(state.selectedMessage);
    } catch (error) {
      el.readerLoading.hidden = true;
      el.readerContent.hidden = true;
      el.readerEmpty.hidden = false;
      el.readerEmpty.querySelector("h2").textContent = "这封信暂时打不开";
      el.readerEmpty.querySelector("p:last-child").textContent = humanError(
        error,
        "请稍后重试。",
      );
    }
  }

  function renderSelectedRow() {
    const selected = entityID(state.selectedMessage);
    el.messageList.querySelectorAll("[data-message-id]").forEach((row) => {
      row.setAttribute(
        "aria-selected",
        String(row.dataset.messageId === selected),
      );
    });
  }

  function renderMessageReader(message) {
    el.readerLoading.hidden = true;
    el.readerEmpty.hidden = true;
    el.readerContent.hidden = false;
    const subject = message.subject || "（无主题）";
    const from =
      message.from ||
      message.headerFrom ||
      message.envelopeFrom ||
      "未知发件人";
    const to = addressList(
      message.to || message.headerTo || message.envelopeTo,
    );
    const cc = addressList(message.cc || message.headerCC);
    const bcc = addressList(message.bcc || message.headerBCC);
    el.readerLabel.textContent = String(
      message.direction || "message",
    ).toUpperCase();
    el.readerSubject.textContent = subject;
    el.readerFrom.textContent = from;
    el.senderAvatar.textContent = getInitials(from);
    el.readerDate.textContent = formatLongDate(messageDate(message));
    el.readerDate.dateTime = messageDateISO(message);
    el.messageStar.setAttribute("aria-pressed", String(isStarred(message)));
    el.messageStar.setAttribute(
      "aria-label",
      isStarred(message) ? "移除星标" : "添加星标",
    );
    const inTrash =
      state.mailbox === "Trash" || String(message.mailbox || "") === "Trash";
    el.messageDelete.setAttribute(
      "aria-label",
      inTrash ? "永久删除这封邮件" : "移到垃圾箱",
    );
    el.readerDetails.hidden = true;
    el.readerDetailsToggle.setAttribute("aria-expanded", "false");
    el.readerDetails.innerHTML = detailsHTML([
      ["发件人", from],
      ["收件人", to || "—"],
      ...(cc ? [["抄送", cc]] : []),
      ...(bcc ? [["密送", bcc]] : []),
      ["日期", formatLongDate(messageDate(message))],
      ["Message-ID", message.messageId || message.rfcMessageId || "—"],
    ]);
    renderAttachments(message);
    renderSafeMessageBody(message, el.readerBody);
    announce(`已打开邮件：${subject}`);
  }

  function renderAttachments(message) {
    const attachments = Array.isArray(message.attachments)
      ? message.attachments
      : [];
    const count = Number(message.attachmentCount || attachments.length || 0);
    if (!count) {
      el.readerAttachments.hidden = true;
      el.readerAttachments.replaceChildren();
      return;
    }
    el.readerAttachments.hidden = false;
    if (!attachments.length) {
      el.readerAttachments.innerHTML = `<span class="attachment-chip">${paperclipIcon()} ${count} 个附件</span>`;
      return;
    }
    el.readerAttachments.innerHTML = attachments
      .map((attachment) => {
        const name = attachment.filename || attachment.name || "附件";
        const meta = attachment.sizeBytes
          ? ` · ${formatBytes(attachment.sizeBytes)}`
          : "";
        const content = `${paperclipIcon()}<span>${escapeHTML(name)}${escapeHTML(meta)}</span>`;
        const attachmentURL =
          attachment.downloadUrl ||
          attachment.url ||
          (entityID(attachment)
            ? `/api/attachments/${encodeURIComponent(entityID(attachment))}`
            : "");
        if (isSafeDownloadURL(attachmentURL)) {
          return `<a class="attachment-chip" href="${escapeHTML(attachmentURL)}" download>${content}</a>`;
        }
        return `<span class="attachment-chip">${content}</span>`;
      })
      .join("");
  }

  function renderSafeMessageBody(message, container) {
    container.replaceChildren();
    const text = message.textBody || message.text || "";
    const html = message.htmlBody || message.html || "";
    if (text) {
      const body = document.createElement("div");
      body.className = "message-plain";
      body.textContent = text;
      container.append(body);
      return;
    }
    if (html) {
      const body = document.createElement("div");
      body.className = "email-html";
      body.innerHTML = sanitizeEmailHTML(html);
      container.append(body);
      return;
    }
    const empty = document.createElement("p");
    empty.textContent = "这封邮件没有可显示的正文。";
    container.append(empty);
  }

  function sanitizeEmailHTML(source) {
    const parser = new DOMParser();
    const doc = parser.parseFromString(String(source), "text/html");
    doc
      .querySelectorAll(
        "script, style, iframe, object, embed, form, input, button, link, meta, base, svg, math, video, audio, source, canvas, marquee",
      )
      .forEach((node) => node.remove());
    doc.querySelectorAll("*").forEach((node) => {
      [...node.attributes].forEach((attribute) => {
        const name = attribute.name.toLowerCase();
        const value = attribute.value.trim();
        if (
          name.startsWith("on") ||
          [
            "style",
            "srcset",
            "formaction",
            "xlink:href",
            "ping",
            "background",
            "poster",
            "srcdoc",
          ].includes(name)
        ) {
          node.removeAttribute(attribute.name);
          return;
        }
        if (
          ["href", "src", "action"].includes(name) &&
          /^(?:javascript|vbscript|file):/i.test(value)
        ) {
          node.removeAttribute(attribute.name);
        }
      });
    });
    doc.querySelectorAll("img").forEach((image) => {
      const src = image.getAttribute("src") || "";
      if (!/^data:image\/(?:png|gif|jpe?g|webp);base64,/i.test(src)) {
        const note = doc.createElement("span");
        note.textContent = image.alt
          ? `[已阻止远程图片：${image.alt}]`
          : "[已阻止远程图片]";
        image.replaceWith(note);
      }
    });
    doc.querySelectorAll("a").forEach((link) => {
      const href = link.getAttribute("href") || "";
      if (!/^(?:https?:|mailto:|#)/i.test(href)) link.removeAttribute("href");
      link.setAttribute("target", "_blank");
      link.setAttribute("rel", "noopener noreferrer nofollow");
    });
    return doc.body.innerHTML;
  }

  async function markMessageSeen(message) {
    const id = entityID(message);
    if (!id) return;
    setSeenLocally(message, true);
    updateMessageSummary(message);
    renderMessages();
    renderSelectedRow();
    try {
      await request(`/messages/${encodeURIComponent(id)}/flags`, {
        method: "POST",
        body: { seen: true },
      });
      loadMailboxes();
    } catch (error) {
      toast(humanError(error, "未读状态同步失败。"), "error");
    }
  }

  async function toggleSelectedStar() {
    const message = state.selectedMessage;
    const id = entityID(message);
    if (!id) return;
    const next = !isStarred(message);
    el.messageStar.disabled = true;
    setStarredLocally(message, next);
    renderMessageReader(message);
    updateMessageSummary(message);
    renderMessages();
    renderSelectedRow();
    try {
      await request(`/messages/${encodeURIComponent(id)}/flags`, {
        method: "POST",
        body: { starred: next },
      });
      toast(next ? "已添加星标。" : "已移除星标。", "success");
    } catch (error) {
      setStarredLocally(message, !next);
      renderMessageReader(message);
      renderMessages();
      renderSelectedRow();
      toast(humanError(error, "星标同步失败。"), "error");
    } finally {
      el.messageStar.disabled = false;
    }
  }

  async function deleteSelectedMessage() {
    const message = state.selectedMessage;
    const id = entityID(message);
    if (!id) return;
    const inTrash =
      state.mailbox === "Trash" || String(message.mailbox || "") === "Trash";
    if (inTrash) {
      const approved = await confirmAction({
        title: "从你的邮箱中永久删除？",
        description:
          "删除后你将无法再查看或搜索这封邮件。按照系统留存规则，管理员归档仍可能保留邮件内容。",
        acceptLabel: "永久删除",
      });
      if (!approved) return;
    }

    el.messageDelete.disabled = true;
    try {
      if (inTrash) {
        await request(`/messages/${encodeURIComponent(id)}/expunge`, {
          method: "POST",
          body: {},
        });
        toast("邮件已从你的邮箱永久删除。", "success");
      } else {
        await request(`/messages/${encodeURIComponent(id)}/move`, {
          method: "POST",
          body: { mailbox: "Trash" },
        });
        toast("邮件已移到垃圾箱。", "success");
      }
      clearReader();
      await Promise.allSettled([loadMessages(), loadMailboxes(), refreshMe()]);
    } catch (error) {
      toast(humanError(error, "删除邮件失败。"), "error");
    } finally {
      el.messageDelete.disabled = false;
    }
  }

  function clearReader() {
    state.selectedMessage = null;
    el.readerLoading.hidden = true;
    el.readerContent.hidden = true;
    el.readerEmpty.hidden = false;
    el.mailView.classList.remove("reader-open");
    renderSelectedRow();
  }

  function closeReaderOnMobile() {
    el.mailView.classList.remove("reader-open");
    const selected = el.messageList.querySelector('[aria-selected="true"]');
    selected?.focus();
  }

  function toggleReaderDetails() {
    const expanded =
      el.readerDetailsToggle.getAttribute("aria-expanded") === "true";
    el.readerDetailsToggle.setAttribute("aria-expanded", String(!expanded));
    el.readerDetails.hidden = expanded;
  }

  function updateMessageSummary(message) {
    const id = entityID(message);
    const index = state.messages.findIndex((item) => entityID(item) === id);
    if (index >= 0)
      state.messages[index] = { ...state.messages[index], ...message };
  }

  function handleMailboxNavigation(event) {
    const button = event.target.closest("[data-mailbox]");
    if (!button) return;
    selectMailbox(button.dataset.mailbox);
  }

  function selectMailbox(mailbox) {
    state.mailbox = mailbox;
    state.page = 1;
    state.query = "";
    el.searchInput.value = "";
    el.mailboxKicker.textContent =
      mailbox === "INBOX" ? "LATEST DELIVERY" : "MAILBOX";
    el.mailboxTitle.textContent = mailboxLabel(mailbox);
    el.mailNavigation.querySelectorAll("[data-mailbox]").forEach((button) => {
      button.classList.toggle("active", button.dataset.mailbox === mailbox);
    });
    setMode("mail", { load: false });
    clearReader();
    closeSidebar();
    loadMessages();
  }

  function handleSearch(event) {
    event.preventDefault();
    state.query = el.searchInput.value.trim();
    state.page = 1;
    closeMobileSearch();
    loadMessages();
  }

  function handleModeSwitch(event) {
    const button = event.target.closest("[data-mode]");
    if (button) setMode(button.dataset.mode);
  }

  function setMode(mode, { load = true } = {}) {
    if (
      mode === "admin" &&
      String(state.me?.role || "").toLowerCase() !== "admin"
    ) {
      toast("只有管理员可以打开管理端。", "error");
      return;
    }
    state.mode = mode;
    const admin = mode === "admin";
    el.mailView.hidden = admin;
    el.adminView.hidden = !admin;
    el.mailNavigation.hidden = admin;
    el.adminNavigation.hidden = !admin;
    el.quotaCard.hidden = admin;
    el.composeTrigger.hidden = admin;
    el.globalSearch.hidden = admin;
    el.modeSwitch.querySelectorAll("[data-mode]").forEach((button) => {
      button.classList.toggle("active", button.dataset.mode === mode);
    });
    document.querySelectorAll("[data-mobile-action]").forEach((button) => {
      button.classList.toggle(
        "active",
        (button.dataset.mobileAction === "admin") === admin &&
          ["admin", "inbox"].includes(button.dataset.mobileAction),
      );
    });
    const mobileCompose = document.querySelector(
      '[data-mobile-action="compose"]',
    );
    mobileCompose.hidden = admin;
    document
      .querySelector(".mobile-navigation")
      ?.classList.toggle("admin-active", admin);
    closeSidebar();
    if (admin && load) loadAdminSection(state.adminSection);
  }

  function handleAdminNavigation(event) {
    const button = event.target.closest("[data-admin-section]");
    if (!button) return;
    loadAdminSection(button.dataset.adminSection);
    closeSidebar();
  }

  async function loadAdminSection(section) {
    const config = ADMIN_SECTIONS[section];
    if (!config) return;
    state.adminSection = section;
    setMode("admin", { load: false });
    el.adminNavigation
      .querySelectorAll("[data-admin-section]")
      .forEach((button) => {
        button.classList.toggle(
          "active",
          button.dataset.adminSection === section,
        );
      });
    el.adminKicker.textContent =
      section === "archive" ? "PRIVACY CONTROLLED" : "ADMIN DESK";
    el.adminTitle.textContent = config.title;
    el.adminDescription.textContent = config.description;
    el.adminPrimaryAction.hidden = !config.action;
    el.adminPrimaryAction.textContent = config.action || "";
    el.adminContent.innerHTML = stateLoading("正在读取管理数据…");
    const requestNumber = ++state.adminRequest;
    try {
      const paged = section === "archive" || section === "audit";
      const page = paged
        ? Math.max(1, Number(state.adminPages[section] || 1))
        : 1;
      const endpoint = paged
        ? `${config.endpoint}?page=${page}&limit=50`
        : config.endpoint;
      let payload = await request(endpoint);
      if (section === "domains") payload = await enrichDomainPayload(payload);
      if (requestNumber !== state.adminRequest) return;
      state.adminData = payload;
      renderAdminSection(section, payload);
    } catch (error) {
      if (requestNumber !== state.adminRequest) return;
      el.adminContent.innerHTML = stateError(
        humanError(error, "管理数据读取失败。"),
        "admin",
      );
    }
  }

  function renderAdminSection(section, payload) {
    switch (section) {
      case "overview":
        renderAdminOverview(payload);
        break;
      case "domains":
        renderAdminDomains(payload);
        break;
      case "users":
        renderAdminUsers(payload);
        break;
      case "aliases":
        renderAdminAliases(payload);
        break;
      case "archive":
        renderAdminArchive(payload);
        break;
      case "queue":
        renderAdminQueue(payload);
        break;
      case "audit":
        renderAdminAudit(payload);
        break;
      default:
        el.adminContent.innerHTML = stateError("未知的管理页面。", "admin");
    }
  }

  async function enrichDomainPayload(payload) {
    const domains = normalizeList(payload, ["domains", "items"]);
    const items = await Promise.all(
      domains.map(async (domain) => {
        const id = entityID(domain);
        if (!id) return domain;
        try {
          const detail = await request(
            `/admin/domains/${encodeURIComponent(id)}/dns`,
          );
          return {
            ...domain,
            ...(detail.domain || {}),
            records: Array.isArray(detail.records)
              ? detail.records
              : domain.records,
          };
        } catch (_) {
          return domain;
        }
      }),
    );
    return { ...(payload || {}), items };
  }

  function renderAdminOverview(payload) {
    const stats = normalizeEntity(payload, ["stats", "data"]);
    const cards = [
      ["邮件域名", numberValue(stats.domains), "已加入系统"],
      [
        "活跃邮箱",
        numberValue(stats.activeUsers ?? stats.users),
        `共 ${numberValue(stats.users)} 个账号`,
      ],
      [
        "归档邮件",
        numberValue(stats.messages ?? stats.archivedMessages),
        `${formatBytes(stats.archivedBytes || 0)} MIME · ${formatBytes(stats.estimatedStorageBytes || 0)} 估算占用`,
      ],
      [
        "等待投递",
        numberValue(stats.queued),
        `${numberValue(stats.failed)} 个失败`,
      ],
    ];
    el.adminContent.innerHTML = `
      <div class="stats-grid">
        ${cards.map((card, index) => `<article class="stat-card${index === 3 && Number(stats.failed) > 0 ? " danger-stat" : ""}"><span>${escapeHTML(card[0])}</span><strong>${escapeHTML(card[1])}</strong><small>${escapeHTML(card[2])}</small></article>`).join("")}
      </div>
      <section class="admin-notice">
        <span class="notice-number" aria-hidden="true">!</span>
        <div><h2>管理员可见不等于无痕访问</h2><p>用户永久删除后的邮件仍由系统归档保留。进入邮件正文前必须填写理由，每次查看都会写入审计记录。</p></div>
      </section>`;
  }

  function renderAdminDomains(payload) {
    const domains = normalizeList(payload, ["domains", "items"]);
    if (!domains.length) {
      el.adminContent.innerHTML = adminEmpty(
        "还没有邮件域名",
        "添加域名后，系统会给出逐项 DNS 配置。",
        "添加域名",
      );
      return;
    }
    el.adminContent.innerHTML = `<div class="dns-stack">${domains.map(renderDomainCard).join("")}</div>`;
  }

  function renderDomainCard(domain) {
    const name = domain.name || domain.domain || "—";
    const status = String(
      domain.status || (domain.verifiedAt ? "verified" : "pending"),
    );
    const records = Array.isArray(domain.records)
      ? domain.records
      : inferredDNSRecords(domain);
    return `
      <article class="dns-card">
        <header><span class="status-badge ${statusClass(status)}">${escapeHTML(statusLabel(status))}</span><h2>${escapeHTML(name)}</h2><p>${domain.verifiedAt ? `验证于 ${escapeHTML(formatLongDate(domain.verifiedAt))}` : "等待 DNS 生效"}</p>${domain.verifiedAt ? "" : `<button class="button button-primary button-small" type="button" data-domain-verify="${escapeHTML(entityID(domain))}">检查 TXT 并验证</button>`}</header>
        <div class="dns-records" aria-label="${escapeHTML(name)} 的 DNS 记录">
          ${records.map((record) => `<div class="dns-record"><strong>${escapeHTML(record.type || "TXT")}</strong><span>${escapeHTML(record.name || record.host || "@")}</span><span>${escapeHTML(record.value || record.content || "—")}</span></div>`).join("")}
        </div>
      </article>`;
  }

  function inferredDNSRecords(domain) {
    const name = domain.name || domain.domain || "example.com";
    const hostname = state.status?.hostname || `mail.${name}`;
    const selector = domain.dkimSelector || "brclio";
    const publicKey = domain.dkimPublicKey || "由服务生成后显示";
    const verification =
      domain.verificationToken || domain.verification || "由服务生成后显示";
    return [
      { type: "TXT", name: `_brclio-mail.${name}`, value: verification },
      { type: "MX", name: "@", value: `10 ${hostname}` },
      { type: "TXT", name: "@", value: "v=spf1 mx -all" },
      {
        type: "TXT",
        name: `${selector}._domainkey`,
        value: publicKey.startsWith("v=DKIM1")
          ? publicKey
          : `v=DKIM1; k=rsa; p=${publicKey}`,
      },
      {
        type: "TXT",
        name: "_dmarc",
        value: `v=DMARC1; p=none; rua=mailto:postmaster@${name}`,
      },
    ];
  }

  function renderAdminUsers(payload) {
    const users = normalizeList(payload, ["users", "items", "mailboxes"]);
    if (!users.length) {
      el.adminContent.innerHTML = adminEmpty(
        "还没有邮箱账号",
        "先创建一个邮箱，再通过 Web 或第三方客户端登录。",
        "新建邮箱",
      );
      return;
    }
    el.adminContent.innerHTML = `
      <section class="data-panel">
        ${dataToolbar(`${users.length} 个邮箱账号`, "筛选邮箱或姓名", "users")}
        <div class="data-table-wrap"><table class="data-table"><thead><tr><th>邮箱</th><th>状态</th><th>空间</th><th>最近登录</th><th>操作</th></tr></thead><tbody>
        ${users
          .map((user) => {
            const status = String(user.status || "active");
            const used = Number(user.usedBytes || 0);
            const quota = Number(user.quotaBytes || 0);
            return `<tr data-filter-row><td><strong>${escapeHTML(user.displayName || user.name || user.email || "—")}</strong><br>${escapeHTML(user.email || "—")}</td><td><span class="status-badge ${statusClass(status)}">${escapeHTML(statusLabel(status))}</span></td><td>${escapeHTML(quota ? `${formatBytes(used)} / ${formatBytes(quota)}` : formatBytes(used))}</td><td>${escapeHTML(user.lastLoginAt ? formatLongDate(user.lastLoginAt) : "尚未登录")}</td><td><div class="table-actions"><button class="button button-quiet button-small" type="button" data-user-status-id="${escapeHTML(entityID(user))}" data-next-status="${status.toLowerCase() === "suspended" ? "active" : "suspended"}">${status.toLowerCase() === "suspended" ? "恢复" : "暂停"}</button></div></td></tr>`;
          })
          .join("")}
        </tbody></table></div>
      </section>`;
  }

  function renderAdminAliases(payload) {
    const aliases = normalizeList(payload, ["aliases", "items"]);
    if (!aliases.length) {
      el.adminContent.innerHTML = adminEmpty(
        "还没有邮箱别名",
        "别名会把邮件送到一个真实邮箱，但本身不能登录。",
        "添加别名",
      );
      return;
    }
    el.adminContent.innerHTML = `
      <section class="data-panel">
        ${dataToolbar(`${aliases.length} 个别名`, "筛选地址或目标邮箱", "aliases")}
        <div class="data-table-wrap"><table class="data-table"><thead><tr><th>别名地址</th><th>投递到</th><th>状态</th><th>创建时间</th></tr></thead><tbody>
        ${aliases.map((alias) => `<tr data-filter-row><td><strong>${escapeHTML(alias.address || alias.email || "—")}</strong></td><td>${escapeHTML(alias.target || alias.targetEmail || "—")}</td><td><span class="status-badge ${alias.enabled === false ? "danger" : "success"}">${alias.enabled === false ? "已停用" : "启用"}</span></td><td>${escapeHTML(formatLongDate(alias.createdAt))}</td></tr>`).join("")}
        </tbody></table></div>
      </section>`;
  }

  function renderAdminArchive(payload) {
    const messages = normalizeList(payload, ["messages", "archive", "items"]);
    if (!messages.length) {
      el.adminContent.innerHTML =
        adminEmpty(
          "归档中还没有邮件",
          "经本系统成功接收或提交发送的邮件会出现在这里。",
          "",
        ) + adminPager("archive", payload);
      return;
    }
    el.adminContent.innerHTML = `
      <section class="data-panel">
        ${dataToolbar(`${messages.length} 封留存邮件`, "筛选发件人、收件人或主题", "archive")}
        <div class="data-table-wrap"><table class="data-table"><thead><tr><th>方向</th><th>发件人与收件人</th><th>主题</th><th>时间</th><th>审计访问</th></tr></thead><tbody>
        ${messages.map((message) => `<tr data-filter-row><td><span class="status-badge ${String(message.direction).toLowerCase() === "outbound" ? "warning" : "success"}">${escapeHTML(directionLabel(message.direction))}</span></td><td><strong>${escapeHTML(message.from || message.headerFrom || message.envelopeFrom || "—")}</strong><br>${escapeHTML(addressList(message.to || message.headerTo || message.envelopeTo) || "—")}</td><td>${escapeHTML(message.subject || "（无主题）")}</td><td>${escapeHTML(formatLongDate(message.createdAt || messageDate(message)))}</td><td><button class="button button-danger button-small archive-row-button" type="button" data-archive-id="${escapeHTML(entityID(message))}">填写理由后查看</button></td></tr>`).join("")}
        </tbody></table></div>
	  </section>${adminPager("archive", payload)}`;
  }

  function renderAdminQueue(payload) {
    const items = normalizeList(payload, ["queue", "items", "deliveries"]);
    if (!items.length) {
      el.adminContent.innerHTML = adminEmpty(
        "投递队列很安静",
        "当前没有等待发送或重试的邮件。",
        "",
      );
      return;
    }
    el.adminContent.innerHTML = `
      <section class="data-panel">
        ${dataToolbar(`${items.length} 个投递任务`, "筛选收件人、状态或错误", "queue")}
        <div class="data-table-wrap"><table class="data-table"><thead><tr><th>收件人</th><th>状态</th><th>尝试</th><th>下次处理</th><th>最近错误</th></tr></thead><tbody>
        ${items.map((item) => `<tr data-filter-row><td><strong>${escapeHTML(item.recipient || "—")}</strong></td><td><span class="status-badge ${statusClass(item.status)}">${escapeHTML(statusLabel(item.status))}</span></td><td>${escapeHTML(numberValue(item.attempts))}</td><td>${escapeHTML(item.nextAttempt ? formatLongDate(item.nextAttempt) : "—")}</td><td>${escapeHTML(item.lastError || "—")}</td></tr>`).join("")}
        </tbody></table></div>
      </section>`;
  }

  function renderAdminAudit(payload) {
    const events = normalizeList(payload, ["audit", "events", "items"]);
    if (!events.length) {
      el.adminContent.innerHTML =
        adminEmpty(
          "还没有审计事件",
          "管理员操作和归档访问会按时间记录在这里。",
          "",
        ) + adminPager("audit", payload);
      return;
    }
    el.adminContent.innerHTML = `<section class="data-panel">${events.map((event) => `<article class="audit-event"><span class="audit-dot" aria-hidden="true"></span><div><strong>${escapeHTML(auditActionLabel(event.action))}</strong><p>${escapeHTML(event.actorEmail || event.actor || "系统")} · ${escapeHTML(event.targetType || "对象")} ${escapeHTML(event.targetId || "")}${event.reason ? ` · 理由：${escapeHTML(event.reason)}` : ""}</p></div><time datetime="${escapeHTML(dateISO(event.createdAt))}">${escapeHTML(formatLongDate(event.createdAt))}</time></article>`).join("")}</section>${adminPager("audit", payload)}`;
  }

  function adminPager(section, payload) {
    const page = Math.max(
      1,
      Number(payload?.page || state.adminPages[section] || 1),
    );
    const hasMore = Boolean(payload?.hasMore ?? payload?.hasNext);
    if (page === 1 && !hasMore) return "";
    return `<nav class="pagination admin-pagination" aria-label="${section === "audit" ? "审计记录" : "留存归档"}分页"><button class="button button-quiet button-small" type="button" data-admin-page="${page - 1}" ${page <= 1 ? "disabled" : ""}>上一页</button><span>第 ${page} 页</span><button class="button button-quiet button-small" type="button" data-admin-page="${page + 1}" ${hasMore ? "" : "disabled"}>下一页</button></nav>`;
  }

  function handleAdminContentClick(event) {
    const pageButton = event.target.closest("[data-admin-page]");
    if (pageButton) {
      const page = Math.max(1, Number(pageButton.dataset.adminPage || 1));
      state.adminPages[state.adminSection] = page;
      loadAdminSection(state.adminSection);
      return;
    }
    const retry = event.target.closest('[data-retry="admin"]');
    if (retry) {
      loadAdminSection(state.adminSection);
      return;
    }
    const emptyAction = event.target.closest("[data-admin-empty-action]");
    if (emptyAction) {
      openAdminCreateDialog();
      return;
    }
    const userStatus = event.target.closest("[data-user-status-id]");
    if (userStatus) {
      patchUserStatus(userStatus);
      return;
    }
    const domainVerify = event.target.closest("[data-domain-verify]");
    if (domainVerify) {
      verifyDomain(domainVerify);
      return;
    }
    const archive = event.target.closest("[data-archive-id]");
    if (archive) openArchiveReason(archive.dataset.archiveId);
  }

  async function verifyDomain(button) {
    const id = button.dataset.domainVerify;
    if (!id) return;
    button.disabled = true;
    button.textContent = "正在查询 DNS…";
    try {
      await request(`/admin/domains/${encodeURIComponent(id)}/verify`, {
        method: "POST",
        body: {},
      });
      toast("域名所有权验证通过。", "success");
      await loadAdminSection("domains");
    } catch (error) {
      button.disabled = false;
      button.textContent = "重新检查 TXT";
      toast(humanError(error, "尚未查询到正确的所有权 TXT 记录。"), "error");
    }
  }

  function handleAdminFilter(event) {
    const input = event.target.closest("[data-admin-filter]");
    if (!input) return;
    const query = input.value.trim().toLocaleLowerCase("zh-CN");
    el.adminContent.querySelectorAll("[data-filter-row]").forEach((row) => {
      row.hidden =
        query && !row.textContent.toLocaleLowerCase("zh-CN").includes(query);
    });
  }

  function openAdminCreateDialog() {
    const section = state.adminSection;
    if (!ADMIN_SECTIONS[section]?.action) return;
    clearFormError(el.adminFormError, el.adminForm);
    el.adminForm.dataset.section = section;
    const definitions = adminFormDefinitions(section);
    el.adminFormKicker.textContent = definitions.kicker;
    el.adminFormTitle.textContent = definitions.title;
    el.adminFormFields.innerHTML = definitions.fields;
    showDialog(el.adminFormDialog);
    requestAnimationFrame(() =>
      el.adminFormFields.querySelector("input, select")?.focus(),
    );
  }

  function adminFormDefinitions(section) {
    if (section === "domains") {
      return {
        kicker: "DOMAIN WIZARD · 01",
        title: "添加邮件域名",
        fields: fieldHTML(
          "domain-name",
          "name",
          "域名",
          "text",
          "example.com",
          { required: true, help: "不要填写协议、路径或 mail. 前缀。" },
        ),
      };
    }
    if (section === "users") {
      return {
        kicker: "NEW POSTBOX",
        title: "新建邮箱账号",
        fields: [
          fieldHTML(
            "user-name",
            "displayName",
            "显示名称",
            "text",
            "例如：王小明",
            { required: true },
          ),
          fieldHTML(
            "user-email",
            "email",
            "邮箱地址",
            "email",
            "name@example.com",
            { required: true },
          ),
          fieldHTML(
            "user-password",
            "password",
            "初始密码",
            "password",
            "至少 12 个字符",
            { required: true, minLength: 12 },
          ),
          fieldHTML(
            "user-quota",
            "quotaMB",
            "空间配额（MiB）",
            "number",
            "1024",
            {
              required: true,
              min: 1,
              value: 1024,
              help: "用户永久删除邮件后释放逻辑配额；管理员归档另行计量。",
            },
          ),
        ].join(""),
      };
    }
    return {
      kicker: "DELIVERY ALIAS",
      title: "添加邮箱别名",
      fields: [
        fieldHTML(
          "alias-address",
          "address",
          "别名地址",
          "email",
          "hello@example.com",
          { required: true },
        ),
        fieldHTML(
          "alias-target",
          "target",
          "投递到",
          "email",
          "owner@example.com",
          { required: true, help: "别名不能登录，只会将邮件送到真实邮箱。" },
        ),
      ].join(""),
    };
  }

  async function submitAdminForm(event) {
    event.preventDefault();
    clearFormError(el.adminFormError, el.adminForm);
    if (!el.adminForm.reportValidity()) return;
    const section = el.adminForm.dataset.section;
    const data = Object.fromEntries(new FormData(el.adminForm));
    if (section === "users") {
      data.quotaBytes = Math.round(Number(data.quotaMB) * 1024 * 1024);
      delete data.quotaMB;
    }
    setFormBusy(el.adminForm, true, "正在保存…");
    try {
      await request(ADMIN_SECTIONS[section].endpoint, {
        method: "POST",
        body: data,
      });
      closeDialog(el.adminFormDialog);
      el.adminForm.reset();
      toast(`${ADMIN_SECTIONS[section].action}成功。`, "success");
      await loadAdminSection(section);
    } catch (error) {
      showFormError(
        el.adminFormError,
        humanError(error, "保存失败，请检查输入。"),
      );
    } finally {
      setFormBusy(el.adminForm, false);
    }
  }

  async function patchUserStatus(button) {
    const id = button.dataset.userStatusId;
    const nextStatus = button.dataset.nextStatus;
    const approved = await confirmAction({
      title: nextStatus === "suspended" ? "暂停这个邮箱？" : "恢复这个邮箱？",
      description:
        nextStatus === "suspended"
          ? "暂停后，用户将无法通过 Web、IMAP 或 SMTP 登录，但历史归档不会删除。"
          : "恢复后，用户可以重新登录和收发邮件。",
      acceptLabel: nextStatus === "suspended" ? "确认暂停" : "确认恢复",
      danger: nextStatus === "suspended",
    });
    if (!approved) return;
    button.disabled = true;
    try {
      await request(`/admin/users/${encodeURIComponent(id)}`, {
        method: "PATCH",
        body: { status: nextStatus },
      });
      toast(
        nextStatus === "suspended" ? "邮箱已暂停。" : "邮箱已恢复。",
        "success",
      );
      await loadAdminSection("users");
    } catch (error) {
      button.disabled = false;
      toast(humanError(error, "账号状态更新失败。"), "error");
    }
  }

  function openArchiveReason(id) {
    const messages = normalizeList(state.adminData, [
      "messages",
      "archive",
      "items",
    ]);
    state.archiveTarget = messages.find(
      (message) => entityID(message) === String(id),
    ) || { id };
    el.archiveReasonForm.reset();
    clearFormError(el.archiveReasonError, el.archiveReasonForm);
    showDialog(el.archiveReasonDialog);
    requestAnimationFrame(() => el.archiveReason.focus());
  }

  async function viewArchivedMessage(event) {
    event.preventDefault();
    clearFormError(el.archiveReasonError, el.archiveReasonForm);
    if (!el.archiveReasonForm.reportValidity()) return;
    const id = entityID(state.archiveTarget);
    const reason = el.archiveReason.value.trim();
    setFormBusy(el.archiveReasonForm, true, "正在记录并查看…");
    try {
      const payload = await request(
        `/admin/archive/${encodeURIComponent(id)}/view`,
        { method: "POST", body: { reason } },
      );
      const message = normalizeMessagePayload(payload);
      state.archiveReason = reason;
      closeDialog(el.archiveReasonDialog);
      renderArchiveMessage({ ...state.archiveTarget, ...message });
      showDialog(el.archiveMessageDialog);
      toast("访问理由已写入审计记录。", "success");
    } catch (error) {
      showFormError(
        el.archiveReasonError,
        humanError(error, "无法打开归档邮件。"),
      );
    } finally {
      setFormBusy(el.archiveReasonForm, false);
    }
  }

  function renderArchiveMessage(message) {
    const subject = message.subject || "（无主题）";
    const attachments = Array.isArray(message.attachments)
      ? message.attachments
      : [];
    const messageID = entityID(message);
    const attachmentMarkup = attachments.length
      ? `<div class="attachment-list">${attachments
          .map((attachment) => {
            const attachmentID = entityID(attachment);
            return `<button class="attachment-chip" type="button" data-archive-attachment="${escapeHTML(attachmentID)}" data-archive-message="${escapeHTML(messageID)}" data-filename="${escapeHTML(attachment.filename || "attachment")}">${paperclipIcon()}<span>${escapeHTML(attachment.filename || "附件")}${attachment.sizeBytes ? ` · ${escapeHTML(formatBytes(attachment.sizeBytes))}` : ""}</span></button>`;
          })
          .join("")}</div>`
      : "";
    el.archiveMessageTitle.textContent = subject;
    const text = message.textBody || message.text || "";
    el.archiveMessageContent.innerHTML = `
      <dl class="archive-message-meta">${detailsHTML([
        [
          "发件人",
          message.from || message.headerFrom || message.envelopeFrom || "—",
        ],
        [
          "收件人",
          addressList(message.to || message.headerTo || message.envelopeTo) ||
            "—",
        ],
        ["日期", formatLongDate(messageDate(message))],
        ["方向", directionLabel(message.direction)],
        ["Message-ID", message.messageId || message.rfcMessageId || "—"],
      ])}</dl>
      ${attachmentMarkup}
      <div id="archive-safe-body" class="archive-message-body"></div>`;
    const body = el.archiveMessageContent.querySelector("#archive-safe-body");
    if (text) {
      body.textContent = text;
    } else if (message.htmlBody || message.html) {
      renderSafeMessageBody(message, body);
    } else {
      body.textContent = "这封归档邮件没有可显示的正文。";
    }
  }

  async function downloadArchivedAttachment(event) {
    const button = event.target.closest("[data-archive-attachment]");
    if (!button) return;
    button.disabled = true;
    try {
      const response = await fetch(
        `${API_BASE}/admin/archive/${encodeURIComponent(button.dataset.archiveMessage)}/attachments/${encodeURIComponent(button.dataset.archiveAttachment)}`,
        {
          method: "POST",
          credentials: "same-origin",
          headers: {
            Accept: "application/octet-stream",
            "Content-Type": "application/json",
          },
          body: JSON.stringify({ reason: state.archiveReason }),
        },
      );
      if (!response.ok) {
        const payload = (response.headers.get("content-type") || "").includes(
          "application/json",
        )
          ? await response.json().catch(() => null)
          : await response.text().catch(() => "");
        throw new APIError(
          apiErrorMessage(payload) || `下载失败（${response.status}）`,
          response.status,
          payload,
        );
      }
      const blob = await response.blob();
      const objectURL = URL.createObjectURL(blob);
      const link = document.createElement("a");
      link.href = objectURL;
      link.download = button.dataset.filename || "attachment";
      document.body.append(link);
      link.click();
      link.remove();
      setTimeout(() => URL.revokeObjectURL(objectURL), 1000);
      toast("附件下载已写入审计记录。", "success");
    } catch (error) {
      toast(humanError(error, "归档附件下载失败。"), "error");
    } finally {
      button.disabled = false;
    }
  }

  function openCompose(prefill = {}) {
    if (state.mode !== "mail") setMode("mail", { load: false });
    resetCompose();
    el.composeTo.value = prefill.to || "";
    el.composeCc.value = prefill.cc || "";
    el.composeBcc.value = prefill.bcc || "";
    el.composeSubject.value = prefill.subject || "";
    el.composeBody.value = prefill.body || "";
    if (prefill.cc || prefill.bcc) setCopyFields(true);
    showDialog(el.composeDialog);
    requestAnimationFrame(() => el.composeTo.focus());
  }

  function replyToSelected() {
    const message = state.selectedMessage;
    if (!message) return;
    const replyTo =
      message.replyTo ||
      message.from ||
      message.headerFrom ||
      message.envelopeFrom ||
      "";
    const subject = String(message.subject || "");
    const replySubject = /^re:/i.test(subject)
      ? subject
      : `Re: ${subject || "（无主题）"}`;
    const quoted = message.textBody
      ? `\n\n—— 原邮件 ——\n${message.textBody}`
      : "";
    openCompose({ to: replyTo, subject: replySubject, body: quoted });
  }

  function toggleCopyFields() {
    setCopyFields(el.copyFields.hidden);
  }

  function setCopyFields(show) {
    el.copyFields.hidden = !show;
    el.toggleCopyFields.setAttribute("aria-expanded", String(show));
    if (show) el.composeCc.focus();
  }

  function resetCompose() {
    clearTimeout(state.draftTimer);
    state.draftTimer = null;
    state.composeGeneration += 1;
    state.draftId = null;
    state.composeFiles = [];
    el.composeForm.reset();
    setCopyFields(false);
    el.composeAttachments.replaceChildren();
    el.draftStatus.textContent = "";
    clearFormError(el.composeError, el.composeForm);
  }

  function handleComposeCancel(event) {
    event.preventDefault();
    if (hasComposeContent()) saveDraft();
    closeDialog(el.composeDialog);
  }

  function scheduleDraftSave() {
    clearTimeout(state.draftTimer);
    state.draftTimer = setTimeout(saveDraft, 1_200);
    el.draftStatus.textContent = hasComposeContent() ? "等待保存草稿…" : "";
  }

  async function saveDraft() {
    if (!hasComposeContent() || !el.composeDialog.open) return;
    const generation = state.composeGeneration;
    const snapshot = composePayload();
    const files = [...state.composeFiles];
    state.draftSavePromise = state.draftSavePromise
      .catch(() => {})
      .then(async () => {
        if (generation !== state.composeGeneration) return;
        el.draftStatus.textContent = "正在保存草稿…";
        try {
          const body = {
            ...snapshot,
            ...(state.draftId ? { id: state.draftId } : {}),
          };
          body.attachments = await Promise.all(files.map(fileToPayload));
          if (generation !== state.composeGeneration) return;
          const payload = await request("/drafts", {
            method: "POST",
            body,
            timeout: 60_000,
          });
          if (generation !== state.composeGeneration) return;
          const draft = normalizeEntity(payload, ["draft", "message", "item"]);
          state.draftId = draft.id || draft.messageId || state.draftId;
          el.draftStatus.textContent = `草稿已保存 · ${new Intl.DateTimeFormat("zh-CN", { hour: "2-digit", minute: "2-digit" }).format(new Date())}`;
          loadMailboxes();
        } catch (error) {
          if (generation === state.composeGeneration)
            el.draftStatus.textContent = "草稿保存失败";
        }
      });
    return state.draftSavePromise;
  }

  function hasComposeContent() {
    return Boolean(
      el.composeTo.value.trim() ||
        el.composeSubject.value.trim() ||
        el.composeBody.value.trim() ||
        state.composeFiles.length,
    );
  }

  function composePayload() {
    return {
      to: splitAddresses(el.composeTo.value),
      cc: splitAddresses(el.composeCc.value),
      bcc: splitAddresses(el.composeBcc.value),
      subject: el.composeSubject.value.trim(),
      body: el.composeBody.value,
      htmlBody: "",
      attachments: [],
    };
  }

  async function sendMessage(event) {
    event.preventDefault();
    clearFormError(el.composeError, el.composeForm);
    if (!el.composeForm.reportValidity()) return;
    const recipients = splitAddresses(el.composeTo.value);
    if (
      !recipients.length ||
      recipients.some((address) => !isEmailLike(address))
    ) {
      showFormError(el.composeError, "请填写有效的收件人地址。", el.composeTo);
      return;
    }
    clearTimeout(state.draftTimer);
    setFormBusy(el.composeForm, true, "正在投递…");
    try {
      await state.draftSavePromise.catch(() => {});
      const payload = composePayload();
      if (state.draftId) payload.draftId = state.draftId;
      payload.attachments = await Promise.all(
        state.composeFiles.map(fileToPayload),
      );
      await request("/compose", {
        method: "POST",
        body: payload,
        timeout: 60_000,
      });
      closeDialog(el.composeDialog);
      resetCompose();
      toast("邮件已交给投递队列。", "success");
      if (state.mailbox === "Sent" || state.mailbox === "Drafts")
        await loadMessages();
      await Promise.allSettled([loadMailboxes(), refreshMe()]);
    } catch (error) {
      showFormError(
        el.composeError,
        humanError(error, "邮件发送失败，请稍后重试。"),
      );
    } finally {
      setFormBusy(el.composeForm, false);
    }
  }

  function addComposeFiles(event) {
    const incoming = [...event.target.files];
    const currentSize = state.composeFiles.reduce(
      (sum, file) => sum + file.size,
      0,
    );
    const accepted = [];
    let total = currentSize;
    for (const file of incoming) {
      if (total + file.size > COMPOSE_FILE_LIMIT) {
        toast(
          `附件总大小不能超过 ${formatBytes(COMPOSE_FILE_LIMIT)}。`,
          "error",
        );
        break;
      }
      accepted.push(file);
      total += file.size;
    }
    state.composeFiles.push(...accepted);
    event.target.value = "";
    renderComposeFiles();
    scheduleDraftSave();
  }

  function renderComposeFiles() {
    el.composeAttachments.innerHTML = state.composeFiles
      .map(
        (file, index) =>
          `<span class="compose-file"><span>${escapeHTML(file.name)} · ${escapeHTML(formatBytes(file.size))}</span><button type="button" data-remove-file="${index}" aria-label="移除附件 ${escapeHTML(file.name)}">×</button></span>`,
      )
      .join("");
  }

  function removeComposeFile(event) {
    const button = event.target.closest("[data-remove-file]");
    if (!button) return;
    state.composeFiles.splice(Number(button.dataset.removeFile), 1);
    renderComposeFiles();
    scheduleDraftSave();
  }

  async function fileToPayload(file) {
    const content = await new Promise((resolve, reject) => {
      const reader = new FileReader();
      reader.onload = () => resolve(String(reader.result).split(",")[1] || "");
      reader.onerror = () => reject(new Error(`无法读取附件：${file.name}`));
      reader.readAsDataURL(file);
    });
    return {
      filename: file.name,
      contentType: file.type || "application/octet-stream",
      contentBase64: content,
    };
  }

  async function openAccount() {
    renderAccountSummary();
    showDialog(el.accountDialog);
    await loadAppPasswords();
  }

  function renderAccountSummary() {
    const name = state.me.displayName || state.me.name || state.me.email;
    const used = Number(state.me.usedBytes || 0);
    const quota = Number(state.me.quotaBytes || 0);
    el.accountSummary.innerHTML = `<span class="account-avatar" aria-hidden="true">${escapeHTML(getInitials(name))}</span><div><strong>${escapeHTML(name)}</strong><span>${escapeHTML(state.me.email || "—")}</span></div><small>${quota ? `${formatBytes(used)} / ${formatBytes(quota)}` : `${formatBytes(used)} 已使用`}</small>`;
  }

  async function loadAppPasswords() {
    el.appPasswordList.innerHTML = stateLoading("正在读取客户端密码…", true);
    try {
      const payload = await request("/app-passwords");
      state.appPasswords = normalizeList(payload, [
        "appPasswords",
        "passwords",
        "items",
      ]);
      renderAppPasswords();
    } catch (error) {
      el.appPasswordList.innerHTML = `<p class="form-error">${escapeHTML(humanError(error, "客户端密码读取失败。"))}</p>`;
    }
  }

  function renderAppPasswords() {
    if (!state.appPasswords.length) {
      el.appPasswordList.innerHTML = `<p class="field-help">还没有客户端密码。为每台设备单独创建一个吧。</p>`;
      return;
    }
    el.appPasswordList.innerHTML = state.appPasswords
      .map((item) => {
        const revoked = Boolean(item.revokedAt);
        const action = revoked
          ? `<span class="status-badge danger">已撤销</span>`
          : `<button class="button button-danger button-small" type="button" data-revoke-password="${escapeHTML(entityID(item))}">撤销</button>`;
        return `<div class="settings-row"><div><strong>${escapeHTML(item.name || "未命名设备")}</strong><small>创建于 ${escapeHTML(formatLongDate(item.createdAt))}${item.lastUsedAt ? ` · 最近使用 ${escapeHTML(formatLongDate(item.lastUsedAt))}` : ""}${revoked ? ` · 撤销于 ${escapeHTML(formatLongDate(item.revokedAt))}` : ""}</small></div>${action}</div>`;
      })
      .join("");
  }

  function clearAppPasswordSecret() {
    el.appPasswordSecret.hidden = true;
    el.appPasswordSecret.replaceChildren();
  }

  function toggleAppPasswordForm() {
    el.appPasswordForm.hidden = !el.appPasswordForm.hidden;
    if (!el.appPasswordForm.hidden) el.appPasswordName.focus();
  }

  async function createAppPassword(event) {
    event.preventDefault();
    if (!el.appPasswordForm.reportValidity()) return;
    setFormBusy(el.appPasswordForm, true, "生成中…");
    try {
      const payload = await request("/app-passwords", {
        method: "POST",
        body: { name: el.appPasswordName.value.trim() },
      });
      const result = normalizeEntity(payload, [
        "appPassword",
        "password",
        "item",
      ]);
      const secret =
        result.secret ||
        result.password ||
        result.token ||
        payload.secret ||
        payload.password;
      el.appPasswordSecret.hidden = false;
      el.appPasswordSecret.innerHTML = `<strong>请现在复制，关闭后不会再次显示：</strong><code>${escapeHTML(secret || "服务未返回可显示的密码")}</code>`;
      el.appPasswordForm.reset();
      el.appPasswordForm.hidden = true;
      await loadAppPasswords();
    } catch (error) {
      toast(humanError(error, "客户端密码创建失败。"), "error");
    } finally {
      setFormBusy(el.appPasswordForm, false);
    }
  }

  async function revokeAppPassword(event) {
    const button = event.target.closest("[data-revoke-password]");
    if (!button) return;
    const approved = await confirmAction({
      title: "撤销这个客户端密码？",
      description:
        "使用它的邮件客户端会立即失去访问权限，需要重新创建密码后才能连接。",
      acceptLabel: "确认撤销",
    });
    if (!approved) return;
    button.disabled = true;
    try {
      await request(
        `/app-passwords/${encodeURIComponent(button.dataset.revokePassword)}`,
        { method: "DELETE" },
      );
      clearAppPasswordSecret();
      toast("客户端密码已撤销。", "success");
      await loadAppPasswords();
    } catch (error) {
      button.disabled = false;
      toast(humanError(error, "撤销失败。"), "error");
    }
  }

  async function logout() {
    el.logoutButton.disabled = true;
    try {
      await request("/auth/logout", {
        method: "POST",
        body: {},
        suppressAuthRedirect: true,
      });
    } catch (error) {
      if (!(error instanceof APIError && error.status === 401))
        toast(humanError(error, "退出请求未完成。"), "error");
    } finally {
      el.logoutButton.disabled = false;
      closeDialog(el.accountDialog);
      resetSession();
      showTopLevel("login");
    }
  }

  async function refreshMe() {
    try {
      const payload = await request("/me");
      state.me = normalizeEntity(payload, ["user", "me", "account"]);
      updateAccountUI();
    } catch (_) {
      // A failed quota refresh should not interrupt the completed user action.
    }
  }

  function toggleSidebar() {
    const open = !document.body.classList.contains("sidebar-open");
    document.body.classList.toggle("sidebar-open", open);
    el.sidebarScrim.hidden = !open;
    el.sidebarToggle.setAttribute("aria-expanded", String(open));
    el.sidebarToggle.setAttribute("aria-label", open ? "关闭导航" : "打开导航");
    syncSidebarAccessibility();
    if (open)
      requestAnimationFrame(() =>
        el.primarySidebar.querySelector("button:not([hidden])")?.focus(),
      );
  }

  function closeSidebar() {
    document.body.classList.remove("sidebar-open");
    el.sidebarScrim.hidden = true;
    el.sidebarToggle.setAttribute("aria-expanded", "false");
    el.sidebarToggle.setAttribute("aria-label", "打开导航");
    syncSidebarAccessibility();
  }

  function syncSidebarAccessibility() {
    const visuallyHidden =
      window.innerWidth <= 900 &&
      !document.body.classList.contains("sidebar-open");
    el.primarySidebar.hidden = visuallyHidden;
    el.primarySidebar.toggleAttribute("inert", visuallyHidden);
    if (visuallyHidden) el.primarySidebar.setAttribute("aria-hidden", "true");
    else el.primarySidebar.removeAttribute("aria-hidden");
  }

  function handleBrandHome() {
    if (state.mode === "admin") loadAdminSection("overview");
    else selectMailbox("INBOX");
  }

  function handleMobileAction(event) {
    const action = event.currentTarget.dataset.mobileAction;
    if (action === "inbox") selectMailbox("INBOX");
    if (action === "search") openMobileSearch();
    if (action === "compose") openCompose();
    if (action === "settings") openAccount();
    if (action === "admin") {
      if (state.mode === "admin") selectMailbox("INBOX");
      else loadAdminSection(state.adminSection);
    }
  }

  function openMobileSearch() {
    if (state.mode === "admin") setMode("mail", { load: false });
    el.globalSearch.hidden = false;
    el.globalSearch.classList.add("mobile-search-open");
    el.searchInput.focus();
  }

  function closeMobileSearch() {
    el.globalSearch.classList.remove("mobile-search-open");
    if (state.mode === "admin") el.globalSearch.hidden = true;
  }

  function handleResize() {
    if (window.innerWidth > 900) {
      closeSidebar();
      closeMobileSearch();
    } else syncSidebarAccessibility();
  }

  function handleGlobalKeydown(event) {
    if (document.body.classList.contains("sidebar-open")) {
      if (event.key === "Escape") {
        event.preventDefault();
        closeSidebar();
        el.sidebarToggle.focus();
        return;
      }
      if (event.key === "Tab") {
        const focusable = [
          ...el.primarySidebar.querySelectorAll(
            "button:not([disabled]):not([hidden]), a[href]",
          ),
        ];
        if (focusable.length) {
          const first = focusable[0];
          const last = focusable[focusable.length - 1];
          if (event.shiftKey && document.activeElement === first) {
            event.preventDefault();
            last.focus();
          } else if (!event.shiftKey && document.activeElement === last) {
            event.preventDefault();
            first.focus();
          }
        }
      }
    }
    if (
      event.key === "Escape" &&
      el.globalSearch.classList.contains("mobile-search-open")
    ) {
      event.preventDefault();
      closeMobileSearch();
      document.querySelector('[data-mobile-action="search"]')?.focus();
      return;
    }
    const target = event.target;
    const typing =
      target instanceof HTMLInputElement ||
      target instanceof HTMLTextAreaElement ||
      target instanceof HTMLSelectElement ||
      target.isContentEditable;
    const openDialog = document.querySelector("dialog[open]");
    if (openDialog || typing) return;
    if (event.key === "/" && state.mode === "mail") {
      event.preventDefault();
      if (window.innerWidth <= 900) openMobileSearch();
      else el.searchInput.focus();
    }
    if ((event.key === "c" || event.key === "C") && state.mode === "mail") {
      event.preventDefault();
      openCompose();
    }
    if (
      (event.key === "r" || event.key === "R") &&
      state.mode === "mail" &&
      state.selectedMessage
    ) {
      event.preventDefault();
      replyToSelected();
    }
    if (["j", "J", "k", "K"].includes(event.key) && state.mode === "mail") {
      const rows = [...el.messageList.querySelectorAll("[data-message-id]")];
      if (!rows.length) return;
      event.preventDefault();
      const current = rows.findIndex(
        (row) => row.getAttribute("aria-selected") === "true",
      );
      const next = ["j", "J"].includes(event.key)
        ? Math.min(rows.length - 1, current + 1)
        : Math.max(0, current <= 0 ? 0 : current - 1);
      rows[next].focus();
      openMessage(rows[next].dataset.messageId, { focusReader: false });
    }
  }

  function handleDocumentClick(event) {
    const closeButton = event.target.closest("[data-close-dialog]");
    if (closeButton) {
      const dialog = document.getElementById(closeButton.dataset.closeDialog);
      if (dialog === el.composeDialog && hasComposeContent()) saveDraft();
      closeDialog(dialog);
      return;
    }
    const confirmButton = event.target.closest("[data-confirm-value]");
    if (confirmButton) {
      if (confirmButton.dataset.confirmValue === "true") resolveConfirm(true);
      else resolveConfirm(false);
    }
  }

  function showDialog(dialog) {
    if (!dialog) return;
    if (typeof dialog.showModal === "function") {
      if (!dialog.open) dialog.showModal();
    } else {
      dialog.setAttribute("open", "");
    }
  }

  function closeDialog(dialog) {
    if (!dialog) return;
    if (typeof dialog.close === "function" && dialog.open) dialog.close();
    else dialog.removeAttribute("open");
  }

  function confirmAction({
    title,
    description,
    acceptLabel = "确认",
    danger = true,
  }) {
    if (state.confirmResolver) state.confirmResolver(false);
    el.confirmTitle.textContent = title;
    el.confirmDescription.textContent = description;
    el.confirmAccept.textContent = acceptLabel;
    el.confirmAccept.className = `button ${danger ? "button-danger" : "button-primary"}`;
    showDialog(el.confirmDialog);
    return new Promise((resolve) => {
      state.confirmResolver = resolve;
    });
  }

  function resolveConfirmTrue(event) {
    event.preventDefault();
    resolveConfirm(true);
  }

  function resolveConfirmFalse(event) {
    event?.preventDefault?.();
    resolveConfirm(false);
  }

  function resolveConfirm(value) {
    closeDialog(el.confirmDialog);
    const resolver = state.confirmResolver;
    state.confirmResolver = null;
    resolver?.(value);
  }

  async function request(path, options = {}) {
    const controller = new AbortController();
    const timeout = setTimeout(
      () => controller.abort(),
      options.timeout || REQUEST_TIMEOUT,
    );
    const headers = new Headers(options.headers || {});
    headers.set("Accept", "application/json");
    const method = String(options.method || "GET").toUpperCase();
    let body = options.body;
    if (!["GET", "HEAD", "OPTIONS"].includes(method) && body === undefined)
      body = {};
    if (
      body !== undefined &&
      body !== null &&
      !(body instanceof FormData) &&
      typeof body !== "string"
    ) {
      headers.set("Content-Type", "application/json");
      body = JSON.stringify(body);
    }
    try {
      const response = await fetch(`${API_BASE}${path}`, {
        method,
        credentials: "same-origin",
        headers,
        body,
        signal: controller.signal,
      });
      const contentType = response.headers.get("content-type") || "";
      let payload = null;
      if (response.status !== 204) {
        if (contentType.includes("application/json")) {
          payload = await response.json().catch(() => null);
        } else {
          payload = await response.text().catch(() => "");
        }
      }
      if (!response.ok) {
        const message =
          apiErrorMessage(payload) || `请求失败（${response.status}）`;
        if (
          response.status === 401 &&
          state.me &&
          !options.suppressAuthRedirect
        )
          handleSessionExpired();
        throw new APIError(message, response.status, payload);
      }
      return payload ?? {};
    } catch (error) {
      if (error.name === "AbortError")
        throw new APIError("请求超时，请检查网络后重试。", 0);
      if (error instanceof APIError) throw error;
      throw new APIError(error.message || "网络连接失败。", 0);
    } finally {
      clearTimeout(timeout);
    }
  }

  function apiErrorMessage(payload) {
    if (!payload) return "";
    if (typeof payload === "string") return payload.trim();
    if (typeof payload.message === "string") return payload.message;
    if (typeof payload.error === "string") return payload.error;
    if (payload.error && typeof payload.error.message === "string")
      return payload.error.message;
    if (typeof payload.detail === "string") return payload.detail;
    return "";
  }

  function handleSessionExpired() {
    if (state.authRedirecting) return;
    state.authRedirecting = true;
    setTimeout(() => {
      resetSession();
      document.querySelectorAll("dialog[open]").forEach(closeDialog);
      showTopLevel("login");
      toast("登录已过期，请重新登录。", "error");
      state.authRedirecting = false;
    }, 0);
  }

  function resetSession() {
    clearTimeout(state.draftTimer);
    state.composeGeneration += 1;
    state.draftTimer = null;
    state.draftId = null;
    state.me = null;
    state.messages = [];
    state.selectedMessage = null;
    state.mailboxes = [];
    state.appPasswords = [];
    state.query = "";
    state.page = 1;
    state.mailbox = "INBOX";
    state.mode = "mail";
    state.composeFiles = [];
    closeSidebar();
  }

  function normalizeEntity(payload, preferredKeys = []) {
    if (!payload || typeof payload !== "object" || Array.isArray(payload))
      return payload || {};
    for (const key of preferredKeys) {
      if (
        payload[key] &&
        typeof payload[key] === "object" &&
        !Array.isArray(payload[key])
      )
        return payload[key];
    }
    if (
      payload.data &&
      typeof payload.data === "object" &&
      !Array.isArray(payload.data)
    ) {
      return normalizeEntity(payload.data, preferredKeys);
    }
    if (
      payload.result &&
      typeof payload.result === "object" &&
      !Array.isArray(payload.result)
    ) {
      return normalizeEntity(payload.result, preferredKeys);
    }
    return payload;
  }

  function normalizeMessagePayload(payload) {
    const message = normalizeEntity(payload, ["message", "item"]);
    const attachments = Array.isArray(payload?.attachments)
      ? payload.attachments
      : Array.isArray(payload?.data?.attachments)
        ? payload.data.attachments
        : Array.isArray(message?.attachments)
          ? message.attachments
          : [];
    return { ...(message || {}), attachments };
  }

  function normalizeList(payload, preferredKeys = []) {
    if (Array.isArray(payload)) return payload;
    if (!payload || typeof payload !== "object") return [];
    for (const key of preferredKeys) {
      if (Array.isArray(payload[key])) return payload[key];
    }
    if (Array.isArray(payload.data)) return payload.data;
    if (payload.data && typeof payload.data === "object") {
      const nested = normalizeList(payload.data, preferredKeys);
      if (
        nested.length ||
        preferredKeys.some((key) => Array.isArray(payload.data[key]))
      )
        return nested;
    }
    if (payload.result && typeof payload.result === "object")
      return normalizeList(payload.result, preferredKeys);
    return [];
  }

  function extractMeta(payload) {
    if (!payload || typeof payload !== "object" || Array.isArray(payload))
      return {};
    return {
      ...(payload.meta || payload.pagination || {}),
      ...Object.fromEntries(
        [
          "page",
          "pages",
          "totalPages",
          "total",
          "count",
          "pageSize",
          "limit",
          "hasNext",
          "hasMore",
        ]
          .filter((key) => payload[key] !== undefined)
          .map((key) => [key, payload[key]]),
      ),
    };
  }

  function entityID(entity) {
    return String(
      entity?.mailboxEntryId ?? entity?.id ?? entity?.messageId ?? "",
    );
  }

  function flagsOf(message) {
    const flags = Array.isArray(message?.flags)
      ? message.flags
      : Array.isArray(message?.userFlags)
        ? message.userFlags
        : [];
    return new Set(
      flags.map((flag) => String(flag).replace(/^\\/, "").toLowerCase()),
    );
  }

  function isSeen(message) {
    if (typeof message?.seen === "boolean") return message.seen;
    if (typeof message?.unread === "boolean") return !message.unread;
    return flagsOf(message).has("seen");
  }

  function isStarred(message) {
    if (typeof message?.starred === "boolean") return message.starred;
    const flags = flagsOf(message);
    return flags.has("flagged") || flags.has("starred");
  }

  function setSeenLocally(message, value) {
    message.seen = value;
    message.unread = !value;
    updateLocalFlag(message, "Seen", value);
  }

  function setStarredLocally(message, value) {
    message.starred = value;
    updateLocalFlag(message, "Flagged", value);
  }

  function updateLocalFlag(message, flag, enabled) {
    const current = Array.isArray(message.flags) ? [...message.flags] : [];
    const index = current.findIndex(
      (item) =>
        String(item).replace(/^\\/, "").toLowerCase() === flag.toLowerCase(),
    );
    if (enabled && index < 0) current.push(`\\${flag}`);
    if (!enabled && index >= 0) current.splice(index, 1);
    message.flags = current;
  }

  function messageListSender(message) {
    if (
      state.mailbox === "Sent" ||
      String(message.mailbox || message.userMailbox || "").toLowerCase() ===
        "sent"
    ) {
      return (
        addressList(message.to || message.headerTo || message.envelopeTo) ||
        "未知收件人"
      );
    }
    return (
      message.from || message.headerFrom || message.envelopeFrom || "未知发件人"
    );
  }

  function messageDate(message) {
    return (
      message?.sentAt ||
      message?.receivedAt ||
      message?.createdAt ||
      message?.date ||
      null
    );
  }

  function messageDateISO(message) {
    return dateISO(messageDate(message));
  }

  function dateISO(value) {
    const date = value ? new Date(value) : null;
    return date && !Number.isNaN(date.getTime()) ? date.toISOString() : "";
  }

  function formatMessageDate(value) {
    const date = value ? new Date(value) : null;
    if (!date || Number.isNaN(date.getTime())) return "—";
    const now = new Date();
    const sameDay = date.toDateString() === now.toDateString();
    return new Intl.DateTimeFormat(
      "zh-CN",
      sameDay
        ? { hour: "2-digit", minute: "2-digit" }
        : { month: "numeric", day: "numeric" },
    ).format(date);
  }

  function formatLongDate(value) {
    const date = value ? new Date(value) : null;
    if (!date || Number.isNaN(date.getTime())) return "—";
    return new Intl.DateTimeFormat("zh-CN", {
      year: "numeric",
      month: "2-digit",
      day: "2-digit",
      hour: "2-digit",
      minute: "2-digit",
    }).format(date);
  }

  function formatBytes(value) {
    const bytes = Number(value || 0);
    if (!Number.isFinite(bytes) || bytes <= 0) return "0 B";
    const units = ["B", "KiB", "MiB", "GiB", "TiB"];
    const index = Math.min(
      units.length - 1,
      Math.floor(Math.log(bytes) / Math.log(1024)),
    );
    const amount = bytes / 1024 ** index;
    return `${amount >= 10 || index === 0 ? amount.toFixed(0) : amount.toFixed(1)} ${units[index]}`;
  }

  function addressList(value) {
    if (Array.isArray(value)) return value.filter(Boolean).join(", ");
    return String(value || "");
  }

  function splitAddresses(value) {
    return String(value || "")
      .split(/[;,\n]/)
      .map((item) => item.trim())
      .filter(Boolean);
  }

  function isEmailLike(value) {
    return /^[^\s<>@]+@[^\s<>@]+\.[^\s<>@]+$/.test(
      String(value).replace(/^.*<([^>]+)>$/, "$1"),
    );
  }

  function isSafeDownloadURL(value) {
    if (!value) return false;
    try {
      const url = new URL(value, window.location.origin);
      return (
        url.origin === window.location.origin &&
        ["http:", "https:"].includes(url.protocol)
      );
    } catch (_) {
      return false;
    }
  }

  function getInitials(value) {
    const clean = String(value || "邮")
      .replace(/<[^>]+>/g, "")
      .trim();
    const beforeAt = clean.split("@")[0] || clean;
    const pieces = beforeAt.split(/[\s._-]+/).filter(Boolean);
    if (pieces.length > 1 && /^[\x00-\x7F]+$/.test(beforeAt))
      return `${pieces[0][0]}${pieces[1][0]}`.toUpperCase();
    return [...beforeAt].slice(0, 2).join("").toUpperCase() || "邮";
  }

  function mailboxLabel(mailbox) {
    return MAILBOX_LABELS[mailbox] || mailbox || "邮件";
  }

  function numberValue(value) {
    const number = Number(value || 0);
    return Number.isFinite(number)
      ? new Intl.NumberFormat("zh-CN").format(number)
      : "0";
  }

  function directionLabel(value) {
    const status = String(value || "").toLowerCase();
    if (["outbound", "out", "sent"].includes(status)) return "外发";
    if (["inbound", "in", "received"].includes(status)) return "入站";
    return value || "未知";
  }

  function statusLabel(value) {
    const status = String(value || "unknown").toLowerCase();
    const labels = {
      active: "正常",
      suspended: "已暂停",
      verified: "已验证",
      pending: "等待中",
      queued: "排队中",
      retrying: "重试中",
      delivered: "已送达",
      sent: "已发送",
      failed: "失败",
      error: "异常",
      enabled: "启用",
      disabled: "停用",
    };
    return labels[status] || value || "未知";
  }

  function statusClass(value) {
    const status = String(value || "").toLowerCase();
    if (
      [
        "active",
        "verified",
        "delivered",
        "sent",
        "enabled",
        "success",
      ].includes(status)
    )
      return "success";
    if (
      ["failed", "error", "suspended", "disabled", "rejected"].includes(status)
    )
      return "danger";
    return "warning";
  }

  function auditActionLabel(value) {
    const action = String(value || "系统事件");
    const labels = {
      "archive.message.view": "查看留存邮件",
      "archive.attachment.download": "下载留存附件",
      "archive.list": "浏览留存归档",
      "user.create": "创建邮箱",
      "user.update": "更新邮箱",
      "alias.create": "创建别名",
      "domain.create": "添加域名",
      "domain.verify": "验证域名",
      "auth.login": "用户登录",
      "auth.logout": "用户退出",
      "app_password.create": "创建应用密码",
      "app_password.revoke": "吊销应用密码",
      "message.submit": "提交邮件",
      "message.receive": "接收邮件",
      "message.move": "移动邮件",
      "message.expunge": "清除个人邮件",
      "message.append": "客户端写入邮件",
      "draft.save": "保存草稿",
      archive_view: "查看留存邮件",
      user_create: "创建邮箱",
      user_update: "更新邮箱",
      alias_create: "创建别名",
      domain_create: "添加域名",
      login: "用户登录",
      logout: "用户退出",
    };
    return labels[action.toLowerCase()] || action;
  }

  function detailsHTML(entries) {
    return entries
      .map(
        ([term, description]) =>
          `<dt>${escapeHTML(term)}</dt><dd>${escapeHTML(description)}</dd>`,
      )
      .join("");
  }

  function dataToolbar(copy, placeholder, id) {
    return `<div class="data-toolbar"><p>${escapeHTML(copy)}</p><label class="data-filter"><span class="sr-only">${escapeHTML(placeholder)}</span><input type="search" data-admin-filter="${escapeHTML(id)}" placeholder="${escapeHTML(placeholder)}"></label></div>`;
  }

  function adminEmpty(title, description, action) {
    return `<div class="empty-state compact"><img src="/assets/private-post-office.png" alt=""><p class="eyebrow">NOTHING HERE YET</p><h2>${escapeHTML(title)}</h2><p>${escapeHTML(description)}</p>${action ? `<button class="button button-primary" type="button" data-admin-empty-action>${escapeHTML(action)}</button>` : ""}</div>`;
  }

  function stateLoading(copy, compact = false) {
    return `<div class="state-panel${compact ? " compact" : ""}" role="status"><span class="spinner" aria-hidden="true"></span><p>${escapeHTML(copy)}</p></div>`;
  }

  function stateError(copy, retry) {
    return `<div class="state-panel"><p class="form-error">${escapeHTML(copy)}</p><button class="button button-quiet button-small" type="button" data-retry="${escapeHTML(retry)}">重试</button></div>`;
  }

  function fieldHTML(id, name, label, type, placeholder, options = {}) {
    const attributes = [
      options.required ? "required" : "",
      options.minLength ? `minlength="${Number(options.minLength)}"` : "",
      options.min !== undefined ? `min="${Number(options.min)}"` : "",
      options.value !== undefined ? `value="${escapeHTML(options.value)}"` : "",
    ]
      .filter(Boolean)
      .join(" ");
    return `<div class="field"><label for="${escapeHTML(id)}">${escapeHTML(label)}</label><input id="${escapeHTML(id)}" name="${escapeHTML(name)}" type="${escapeHTML(type)}" placeholder="${escapeHTML(placeholder)}" ${attributes}>${options.help ? `<p class="field-help">${escapeHTML(options.help)}</p>` : ""}</div>`;
  }

  function starIcon() {
    return `<svg viewBox="0 0 24 24"><path d="m12 3 2.7 5.5 6 .9-4.3 4.2 1 5.9-5.4-2.9-5.4 2.9 1-5.9-4.3-4.2 6-.9L12 3Z"/></svg>`;
  }

  function paperclipIcon() {
    return `<svg aria-hidden="true" viewBox="0 0 24 24"><path d="m8 12 5.7-5.7a3 3 0 0 1 4.3 4.3l-8.5 8.5a5 5 0 0 1-7-7L11 3.6"/></svg>`;
  }

  function setFormBusy(form, busy, label = "处理中…") {
    const submit = form.querySelector('[type="submit"]');
    form
      .querySelectorAll("input, textarea, select, button")
      .forEach((control) => {
        if (!control.hasAttribute("data-keep-enabled")) control.disabled = busy;
      });
    if (!submit) return;
    if (busy) {
      submit.dataset.originalHtml = submit.innerHTML;
      submit.textContent = label;
    } else if (submit.dataset.originalHtml) {
      submit.innerHTML = submit.dataset.originalHtml;
      delete submit.dataset.originalHtml;
    }
  }

  function showFormError(node, message, input = null) {
    node.textContent = message;
    node.hidden = false;
    if (input) {
      input.setAttribute("aria-invalid", "true");
      input.focus();
    }
  }

  function clearFormError(node, form) {
    node.hidden = true;
    node.textContent = "";
    form
      ?.querySelectorAll('[aria-invalid="true"]')
      .forEach((input) => input.removeAttribute("aria-invalid"));
  }

  function humanError(error, fallback) {
    if (error instanceof APIError && error.message) return error.message;
    return error?.message || fallback;
  }

  function toast(message, type = "info", duration = 4_500) {
    const item = document.createElement("div");
    item.className = `toast ${type}`;
    item.setAttribute("role", type === "error" ? "alert" : "status");
    const copy = document.createElement("p");
    copy.textContent = message;
    const close = document.createElement("button");
    close.type = "button";
    close.setAttribute("aria-label", "关闭通知");
    close.textContent = "×";
    close.addEventListener("click", () => item.remove());
    item.append(copy, close);
    el.toastRegion.append(item);
    setTimeout(() => item.remove(), duration);
  }

  function announce(message) {
    el.appLiveRegion.textContent = "";
    requestAnimationFrame(() => {
      el.appLiveRegion.textContent = message;
    });
  }

  function escapeHTML(value) {
    return String(value ?? "").replace(
      /[&<>"]/g,
      (character) =>
        ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" })[character],
    );
  }
})();
