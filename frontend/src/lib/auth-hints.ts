export type AuthValues = {
  username: string;
  password: string;
  otp: string;
  setup_token: string;
  login_entry?: string;
  captcha_id?: string;
  captcha_answer?: string;
};
export function loginHints(hash: string) {
  const values = new URLSearchParams(hash.replace(/^#/, ""));
  const loginEntry = values.get("login_entry") ?? "";
  const temporaryToken = values.get("temporary_login") ?? "";
  if (!values.has("login_entry") && !values.has("temporary_login"))
    return { loginEntry, temporaryToken, remainingHash: hash };
  values.delete("login_entry");
  values.delete("temporary_login");
  return {
    loginEntry,
    temporaryToken,
    remainingHash: values.size ? "#" + values.toString() : "",
  };
}
export function authorizationReturn(search: string): string | undefined {
  const back = new URLSearchParams(search).get("return_to");
  return back?.startsWith("/oauth/authorize?") &&
    !back.startsWith("//") &&
    !back.includes("\\")
    ? back
    : undefined;
}
