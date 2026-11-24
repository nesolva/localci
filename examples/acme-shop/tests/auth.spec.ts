import { expect, signIn, test } from "./fixtures";

test("signing in shows who you are", async ({ page, shop }) => {
  await signIn(page, shop);
  await expect(page.getByTestId("whoami")).toHaveText("Signed in as demo");
});

test("a wrong password is refused", async ({ page, shop }) => {
  await signIn(page, shop, "nope");
  await expect(page.getByRole("alert")).toHaveText("Wrong username or password");
});

test("the cart asks you to sign in first", async ({ page, shop }) => {
  await page.goto(shop.path("cart"));
  await expect(page).toHaveURL(/\/login/);
  await expect(page.getByRole("heading", { name: "Sign in" })).toBeVisible();
});
