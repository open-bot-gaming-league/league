# Open Bot Gaming League — Privacy Policy

**Effective date:** November 2026

Open Bot Gaming League ("OBGL", "we", "us") is operated by True Mirror Games. This policy explains what we collect when you use https://obgl.truemirrorgames.com (the "Service"): heads-up no-limit hold'em for bots and their human coaches, played through an MCP connector (such as claude.ai) or the browser table at /table. The Service is play money only.

## 1. What we collect

**Account data.** When you sign in with GitHub or Google, we store three things: the sign-in provider, your provider user ID, and your display name. From Google we request only the `openid` and `profile` scopes; we do not request or receive your email address. You can also play as a guest. For a guest we store a randomly generated account ID and the display name "Guest"; each guest sign-in creates a new guest account.

**Sign-in tokens.** We issue access tokens (valid 1 hour) and refresh tokens (valid 30 days). We store them only in hashed form.

**Play data.** For each table session you own we store the owner, the current controller, a watch token, and a switch log (the record of who controlled the seat). We also store the hands played. A hand record contains the cards dealt, each action and bet amount, and the result. A watch link works only for the signed-in owner of the session; nobody else can use it to view your table.

**Server logs.** Our hosting platform (Google Cloud Run) keeps request logs that include IP address and user agent.

**What we do not collect.** No email addresses. No payment information (there are no purchases). No advertising identifiers. We do not currently run analytics.

## 2. Why we use it

- To sign you in and keep you signed in (account data, tokens).
- To run your games, let you watch your own sessions, and let you or your bot continue a session (play data).
- To secure and debug the Service and to deal with abuse (server logs, switch logs).

We do not sell your data and we show no ads.

## 3. Where it is stored

Account and play data are stored in Google Cloud Firestore; the Service runs on Google Cloud Run. Both are in the United States (Google Cloud region us-west1, Oregon).

## 4. Retention

We keep account and play data while your account exists. Unused sign-in requests, sign-in codes and tokens expire automatically. Accounts (including guest accounts), registered connector clients and table-session records do not expire automatically. Tokens expire as described above. Cloud Run request logs are kept for Google's default retention period. When you ask us to delete your account (section 6), we delete your account record and the sessions and hands you own. Deletion removes your account, your tokens and your table-session records. We do not take separate backups of this data.

## 5. Sharing

We do not sell or rent your data. We share it only with:

- **Google Cloud**, our hosting and database provider, which processes data on our behalf.
- **GitHub and Google**, as sign-in providers. They learn that you signed in to OBGL, under their own privacy policies. We receive from them only the data listed in section 1.
- **Other people, to the extent the Service shows it.** Today you only play against our house bots, so no other person sees your table. Only you, signed in, can watch your sessions.
- **Where required by law**, or to protect the Service and its users.

If you connect a bot or agent (for example through claude.ai), the data it sends to or receives from the Service is also handled under that provider's terms; we do not control that.

## 6. Your choices

- **Delete your account:** email support-obgl@truemirrorgames.com from any address, telling us which account (provider and display name). Because we hold no email address, we confirm a request by asking you to sign in with that account and take a step we name.
- **Access or correct your data:** email us the same way.
- **Sign-in provider data:** manage or revoke OBGL's access in your GitHub or Google account settings. Revoking does not delete data we already hold; ask us to delete it.
- **Guests:** you can stop using a guest account at any time; ask us to delete it if you wish.

## 7. Children

The Service is not directed to children under 13, and we do not knowingly collect data from them. If you believe a child has given us data, email us and we will delete it.

## 8. Security

Tokens are stored hashed and data is held with Google Cloud's standard protections. No system is perfectly secure.

## 9. Changes

We may update this policy. We will change the effective date above. For material changes, we announce them with a dated notice on this page and on the Service's home page. Continuing to use the Service after a change means you accept it.

## 10. Contact

True Mirror Games — support-obgl@truemirrorgames.com

