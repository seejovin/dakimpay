const assert = require("node:assert/strict");
const {readFileSync} = require("node:fs");
const {join} = require("node:path");
const {chromium} = require("playwright");

async function run() {
    const cfg = JSON.parse(readFileSync(join(process.env.DAKIM_GATEWAY_STATE, "clients.json")));
    const browser = await chromium.launch({executablePath: process.env.DAKIM_CHROMIUM_BIN || undefined});
    try {
        const ctx = await browser.newContext();
        const errors = [];
        ctx.on("page", p => p.on("pageerror", error => errors.push(error.message)));
        const register = await ctx.newPage();
        await register.goto(cfg.gateway_url + "/#token=" + encodeURIComponent(cfg.merchants[0].tokens.merchant));
        await register.locator("#register").waitFor();
        assert.equal(new URL(register.url()).hash, "");
        await register.locator("#sale select[name=channel]").selectOption("hosted");
        for (const [provider, method, name] of [["wechat", "wechat_pay", "WeChat Pay"], ["alipay", "alipay", "Alipay"]]) {
            await register.locator("#sale input[name=amount]").fill("3.21");
            await register.locator("#sale select[name=method]").selectOption(method);
            await register.locator("#submit-sale").click();
            const link = register.locator("#checkout");
            await link.waitFor();
            const origin = cfg.providers[provider].origin;
            assert.equal(new URL(await link.getAttribute("href")).origin, origin);
            const pagePromise = ctx.waitForEvent("page");
            await link.click();
            const buyer = await pagePromise;
            await buyer.getByRole("link", {name: "Open simulated " + name + " wallet"}).waitFor();
            assert.equal(await buyer.getByRole("button", {name: /approve/i}).count(), 0);
            if (method === "wechat_pay") {
                await buyer.waitForFunction(() => {
                    const image = document.querySelector("img");
                    return image && image.complete && image.naturalWidth > 0;
                });
            }
            await buyer.getByRole("link", {name: "Open simulated " + name + " wallet"}).click();
            assert.equal(new URL(buyer.url()).origin, origin);
            assert.match(new URL(buyer.url()).pathname, /^\/wallet\//);
            await buyer.getByRole("button", {name: "Approve in virtual wallet"}).click();
            await buyer.getByText("Payment approved", {exact: true}).waitFor();
            await register.waitForFunction(() => document.getElementById("status").textContent === "captured");
            await buyer.close();
        }
        assert.deepEqual(errors, []);
        console.log("PASS Chromium merchant register to WeChat Pay QR/wallet and Alipay handoff; completed both purchases in separate provider origins");
    } finally {
        await browser.close();
    }
}
run().catch(e => { console.error(e.message); process.exitCode = 1; });
