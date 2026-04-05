(function () {
    const ATTRS = {
        get: "hx-get",
        post: "hx-post",
        put: "hx-put",
        delete: "hx-delete",
        trigger: "hx-trigger",
        swap: "hx-swap",
        target: "hx-target",
        vals: "hx-vals",
        indicator: "hx-indicator",
    };

    function getSwapStrategy(el) {
        return el.getAttribute(ATTRS.swap) || "innerHTML";
    }

    function getTarget(el) {
        const sel = el.getAttribute(ATTRS.target);
        if (sel) return document.querySelector(sel);
        return el;
    }

    function doSwap(target, html, strategy) {
        switch (strategy) {
            case "innerHTML":
                target.innerHTML = html;
                break;
            case "outerHTML":
                target.outerHTML = html;
                break;
            case "beforebegin":
                target.insertAdjacentHTML("beforebegin", html);
                break;
            case "afterbegin":
                target.insertAdjacentHTML("afterbegin", html);
                break;
            case "beforeend":
                target.insertAdjacentHTML("beforeend", html);
                break;
            case "afterend":
                target.insertAdjacentHTML("afterend", html);
                break;
            case "none":
                break;
            default:
                target.innerHTML = html;
        }
    }

    function getMethod(el) {
        for (const [method, attr] of Object.entries(ATTRS)) {
            if (["trigger", "swap", "target", "vals", "indicator"].includes(method)) continue;
            if (el.hasAttribute(attr)) return {method: method.toUpperCase(), url: el.getAttribute(attr)};
        }
        return null;
    }

    function collectFormData(el) {
        const form = el.closest("form");
        if (form) return new URLSearchParams(new FormData(form)).toString();
        return "";
    }

    async function makeRequest(el) {
        const req = getMethod(el);
        if (!req) return;

        // Show indicator
        const indicatorSel = el.getAttribute(ATTRS.indicator);
        const indicator = indicatorSel ? document.querySelector(indicatorSel) : null;
        if (indicator) indicator.classList.add("htmx-request");

        const opts = {method: req.method, headers: {"HX-Request": "true"}};

        let url = req.url;

        if (req.method === "POST" || req.method === "PUT") {
            const body = collectFormData(el);

            // Merge hx-vals (JSON)
            const valsAttr = el.getAttribute(ATTRS.vals);
            let extra = {};
            if (valsAttr) {
                try {
                    extra = JSON.parse(valsAttr);
                } catch (e) {
                    console.warn("minihtmx: invalid hx-vals JSON", valsAttr);
                }
            }

            const params = new URLSearchParams(body);
            for (const [k, v] of Object.entries(extra)) params.set(k, v);

            opts.headers["Content-Type"] = "application/x-www-form-urlencoded";
            opts.body = params.toString();
        } else {
            // GET/DELETE — merge form data AND hx-vals into query params
            const u = new URL(url, window.location.origin);

            // Include form data for GET/DELETE too
            const formBody = collectFormData(el);
            if (formBody) {
                const formParams = new URLSearchParams(formBody);
                for (const [k, v] of formParams) u.searchParams.set(k, v);
            }

            const valsAttr = el.getAttribute(ATTRS.vals);
            if (valsAttr) {
                try {
                    const extra = JSON.parse(valsAttr);
                    for (const [k, v] of Object.entries(extra)) u.searchParams.set(k, v);
                } catch (e) {
                    console.warn("minihtmx: invalid hx-vals JSON", valsAttr);
                }
            }
            url = u.toString();
        }

        const target = getTarget(el);
        const strategy = getSwapStrategy(el);

        try {
            const resp = await fetch(url, opts);
            const html = await resp.text();

            if (strategy === "outerHTML") {
                // For outerHTML, we need to find the parent to re-process
                const parent = target.parentElement;
                doSwap(target, html, strategy);
                if (parent) process(parent);
            } else {
                doSwap(target, html, strategy);
                process(target);
            }
        } catch (err) {
            console.error("minihtmx error:", err);
        } finally {
            if (indicator) indicator.classList.remove("htmx-request");
        }
    }

    function getTriggerEvent(el) {
        const attr = el.getAttribute(ATTRS.trigger);
        if (attr) return attr.split(" ")[0];
        // Defaults
        if (el.tagName === "FORM") return "submit";
        if (el.tagName === "INPUT" || el.tagName === "SELECT" || el.tagName === "TEXTAREA") return "change";
        return "click";
    }

    const processed = new WeakSet();

    function process(root) {
        const selector = `[${ATTRS.get}],[${ATTRS.post}],[${ATTRS.put}],[${ATTRS.delete}]`;
        const elements = root.querySelectorAll ? [...(root.matches?.(selector) ? [root] : []), ...root.querySelectorAll(selector),] : [];

        for (const el of elements) {
            if (processed.has(el)) continue;
            processed.add(el);

            const event = getTriggerEvent(el);

            // Special: "load" trigger
            if (event === "load") {
                makeRequest(el);
                continue;
            }

            // Special: "every Xs" polling
            const triggerAttr = el.getAttribute(ATTRS.trigger) || "";
            const pollMatch = triggerAttr.match(/every\s+(\d+)(s|ms)/);
            if (pollMatch) {
                const interval = pollMatch[2] === "s" ? pollMatch[1] * 1000 : +pollMatch[1];
                setInterval(() => makeRequest(el), interval);
                makeRequest(el);
                continue;
            }

            el.addEventListener(event, (e) => {
                if (event === "submit") e.preventDefault();
                makeRequest(el);
            });
        }
    }

    // Initial processing
    if (document.readyState === "loading") {
        document.addEventListener("DOMContentLoaded", () => process(document.body));
    } else {
        process(document.body);
    }
})();