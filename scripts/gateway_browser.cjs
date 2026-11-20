const assert = require("node:assert/strict");
const {readFileSync} = require("node:fs");
const {join} = require("node:path");
const {chromium} = require("playwright");

async function check() {
    const cfg = JSON.parse(readFileSync(join(process.env.DAKIM_GATEWAY_STATE, "clients.json")));
    const account = cfg.merchants[0];
    const browser = await chromium.launch({executablePath: process.env.DAKIM_CHROMIUM_BIN || undefined});
    try {
        const context = await browser.newContext({viewport: {width: 1280, height: 850}});
        const register = await context.newPage();
        const reader = await context.newPage();
        const errors = [];
        context.on("page", page => page.on("pageerror", error => errors.push(error.message)));
        for (const page of [register, reader]) page.on("pageerror", error => errors.push(error.message));
        await register.goto(cfg.gateway_url + "/#token=" + encodeURIComponent(account.tokens.merchant));
        await reader.goto(cfg.rail_url + "/terminal#token=" + encodeURIComponent(account.device_token));
        await register.locator("#register").waitFor();
        await reader.locator("#reader").waitFor();
        assert.equal(new URL(register.url()).hash, "");
        assert.equal(new URL(reader.url()).hash, "");

        const amount = register.locator("#sale input[name=amount]");
        const create = register.locator("#submit-sale");
        const captured = () => register.waitForFunction(() => document.querySelector("#status").textContent === "captured");
        await amount.fill("15.62");
        await create.click();
        await reader.locator("#approve").waitFor();
        assert.match(await reader.locator("#amount").textContent(), /15\.62/);
        await reader.locator("#approve").click();
        await captured();
        await register.locator("#refund input").fill("2.31");
        await register.locator("#refund button").click();
        await register.waitForFunction(() => document.querySelector("#refunded").textContent.includes("2.31"));

        await register.locator("#sale select[name=channel]").selectOption("hosted");
        await amount.fill("3.80");
        await create.click();
        const link = register.locator("#checkout");
        await link.waitFor();
        assert.equal(new URL(await link.getAttribute("href")).origin, cfg.rail_url);
        const popup = context.waitForEvent("page");
        await link.click();
        const buyer = await popup;
        const approve = buyer.getByRole("button", {name: "Approve test payment"});
        await approve.waitFor();
        const submission = buyer.waitForRequest(request => request.method() === "POST");
        await approve.click();
        const headers = await (await submission).allHeaders();
        assert.equal(headers.origin, cfg.rail_url);
        if (headers.referer) assert.equal(new URL(headers.referer).pathname, "/");
        await buyer.getByText("Payment approved", {exact: true}).waitFor();
        await captured();
        await register.setViewportSize({width: 390, height: 850});
        assert.equal(await register.evaluate(() => document.documentElement.scrollWidth > innerWidth), false);
        assert.deepEqual(errors, []);
        console.log("PASS Chromium register, reader, partial refund, hosted form, credential handoff and narrow layout");
    } finally {
        await browser.close();
    }
}

check().catch(error => {console.error(error.message); process.exitCode = 1;});
