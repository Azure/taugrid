// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Minimal DOM contract for the production script's component tests. This is not
// a layout engine; in particular, these tests make no browser/a11y-layout claims.
export function createDOM() {
  let document;
  class Node {
    constructor(tagName = "", data = "") {
      this.tagName = tagName.toUpperCase();
      this.data = data;
      this.childNodes = [];
      this.attributes = new Map();
      this.dataset = {};
      this.listeners = new Map();
      this.parentNode = null;
      this.scrollTop = this.scrollLeft = 0;
      this.selectionStart = this.selectionEnd = null;
      this.className = "";
      this.style = {};
      this.open = this.checked = this.disabled = this.hidden = false;
      this.classList = {
        add: (...names) => { this.className = [...new Set([...this.className.split(" ").filter(Boolean), ...names])].join(" "); },
        remove: (...names) => { this.className = this.className.split(" ").filter((name) => !names.includes(name)).join(" "); },
        contains: (name) => this.className.split(" ").includes(name),
        toggle: (name, force) => {
          const enabled = force ?? !this.classList.contains(name);
          if (enabled) this.classList.add(name); else this.classList.remove(name);
          return enabled;
        },
      };
    }
    get children() { return this.childNodes.filter((node) => node.tagName); }
    get firstChild() { return this.childNodes[0] || null; }
    get textContent() { return this.tagName ? this.childNodes.map((node) => node.textContent).join("") : this.data; }
    set textContent(value) { this.replaceChildren(new Node("", String(value))); }
    get value() {
      if (this.tagName === "SELECT") {
        if (this._value !== undefined) return this.querySelectorAll("option").some((option) => option.value === this._value) ? this._value : "";
        const options = this.querySelectorAll("option");
        return (options.find((option) => option.selected) || options[0])?.value || "";
      }
      return this._value ?? this.attributes.get("value") ?? "";
    }
    set value(value) { this._value = String(value); }
    get form() {
      let node = this.parentNode;
      while (node && node.tagName !== "FORM") node = node.parentNode;
      return node;
    }
    setAttribute(name, value) {
      this.attributes.set(name, String(value));
      if (name === "class") this.className = String(value);
      if (["open", "checked", "disabled", "hidden", "selected"].includes(name)) this[name] = true;
      if (name.startsWith("data-")) this.dataset[name.slice(5).replace(/-([a-z])/g, (_, letter) => letter.toUpperCase())] = String(value);
      // Deliberately do not make setAttribute("value") select an option.
    }
    getAttribute(name) { return name === "class" ? this.className : this.attributes.get(name) ?? null; }
    removeAttribute(name) { this.attributes.delete(name); }
    append(...nodes) {
      for (const node of nodes) this.insertBefore(node instanceof Node ? node : new Node("", String(node)), null);
    }
    insertBefore(node, before) {
      if (node === before) return node;
      node.remove();
      const index = before ? this.childNodes.indexOf(before) : this.childNodes.length;
      if (index < 0) throw new Error("insertBefore reference is not a child");
      this.childNodes.splice(index, 0, node);
      node.parentNode = this;
      return node;
    }
    removeChild(node) {
      const index = this.childNodes.indexOf(node);
      if (index < 0) throw new Error("removeChild target is not a child");
      if (node.contains(document.activeElement)) document.activeElement = null;
      this.childNodes.splice(index, 1);
      node.parentNode = null;
      return node;
    }
    remove() { this.parentNode?.removeChild(this); }
    replaceChildren(...nodes) {
      for (const child of [...this.childNodes]) this.removeChild(child);
      this.append(...nodes);
    }
    contains(node) { return node === this || this.childNodes.some((child) => child.contains(node)); }
    addEventListener(name, listener) {
      if (!this.listeners.has(name)) this.listeners.set(name, []);
      this.listeners.get(name).push(listener);
    }
    dispatch(name, extra = {}) {
      const event = { target: this, preventDefault() {}, stopPropagation() { this.stopped = true; }, ...extra };
      let node = this;
      while (node) {
        event.currentTarget = node;
        for (const listener of node.listeners.get(name) || []) listener(event);
        if (event.stopped) break;
        node = node.parentNode;
      }
      return event;
    }
    focus() { document.activeElement = this; }
    setSelectionRange(start, end) { this.selectionStart = start; this.selectionEnd = end; }
    getBoundingClientRect() { return { width: 800, height: 320, left: 0, top: 0 }; }
    matches(selector) {
      const tag = selector.match(/^[\w-]+/)?.[0];
      if (tag && tag.toUpperCase() !== this.tagName) return false;
      for (const [, name] of selector.matchAll(/\.([\w-]+)/g)) if (!this.classList.contains(name)) return false;
      const id = selector.match(/#([\w-]+)/)?.[1];
      if (id && this.getAttribute("id") !== id) return false;
      for (const [, name, value] of selector.matchAll(/\[([\w-]+)(?:=["']?([^"'\]]+)["']?)?\]/g)) {
        if (this.getAttribute(name) === null) return false;
        if (value !== undefined && this.getAttribute(name) !== value) return false;
      }
      return true;
    }
    querySelectorAll(selector) {
      const results = [];
      const matches = (node, branch) => {
        const parts = branch.trim().split(/\s+/);
        let index = parts.length - 1;
        if (!node.matches(parts[index--])) return false;
        let ancestor = node.parentNode;
        while (index >= 0) {
          if (parts[index] === ">") {
            index--;
            if (!ancestor?.matches(parts[index--])) return false;
            ancestor = ancestor.parentNode;
          } else {
            while (ancestor && !ancestor.matches(parts[index])) ancestor = ancestor.parentNode;
            if (!ancestor) return false;
            ancestor = ancestor.parentNode;
            index--;
          }
        }
        return true;
      };
      const visit = (node) => {
        for (const child of node.children) {
          if (selector.split(",").some((branch) => matches(child, branch))) results.push(child);
          visit(child);
        }
      };
      visit(this);
      return results;
    }
    querySelector(selector) { return this.querySelectorAll(selector)[0] || null; }
  }
  const root = new Node("div");
  root.setAttribute("id", "stellar-root");
  document = {
    activeElement: null, visibilityState: "visible", readyState: "complete",
    getElementById: (id) => id === "stellar-root" ? root : null,
    querySelector: (selector) => root.querySelector(selector),
    querySelectorAll: (selector) => root.querySelectorAll(selector),
    createElement: (tag) => new Node(tag),
    createElementNS: (_, tag) => new Node(tag),
    createTextNode: (text) => new Node("", text),
    addEventListener() {},
  };
  class FormData extends Map {
    constructor(form) {
      super(form.querySelectorAll("input, select, textarea")
        .filter((node) => node.getAttribute("name") && !node.disabled)
        .map((node) => [node.getAttribute("name"), node.value]));
    }
  }
  return { root, document, Node, FormData };
}
