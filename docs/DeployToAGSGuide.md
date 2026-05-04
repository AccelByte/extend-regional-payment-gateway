# Deploy to AGS Extend

This guide walks you through building, uploading, and deploying the payment gateway service to AccelByte Game Services (AGS) Extend.

> **Official reference:** [AGS Extend — Service Extension](https://docs.accelbyte.io/gaming-services/modules/foundations/extend/service-extension/)

---

## Prerequisites

Before you start, make sure you have:

- **Docker 4.30+** installed and running
- **[extend-helper-cli](https://github.com/AccelByte/extend-helper-cli/releases)** installed and available on your `PATH`
- **AGS Admin Portal** access with permission to create and manage Extend apps in your namespace
- **AccelByte M2M client credentials** — `AB_CLIENT_ID`, `AB_CLIENT_SECRET`, `AB_NAMESPACE` (from the Admin Portal under IAM > OAuth Clients)

---

## Step 1 — Build the Docker Image

From the root of the repository, build the container image:

```bash
docker build -t extend-regional-payment-gateway:latest .
```

Verify the image was built:

```bash
docker images extend-regional-payment-gateway
```

---

## Step 2 — Create the App in the AGS Admin Portal

1. Log in to the **AGS Admin Portal** for your environment.
2. Navigate to **Foundations > Extend > Service Extension**.
3. Click **Create App**.
4. Enter a name for the app (e.g. `regional-payment-gateway`) and confirm.
5. Wait for the status to change from `provisioning in progress` to **`undeployed`**. This means the app slot is ready to receive an image.

> If the status shows `provisioning failed` or `provisioning timeout`, check your namespace permissions or contact AccelByte support.

---

## Step 3 — Upload the Container Image

Use `extend-helper-cli` to authenticate against the AGS container registry and push the image.

### 3a — Log in to the AGS registry

```bash
extend-helper-cli dockerLogin \
  --namespace <your-namespace> \
  --clientId <AB_CLIENT_ID> \
  --clientSecret <AB_CLIENT_SECRET>
```

The command prints the registry URL. Copy it — you need it in the next step.

### 3b — Tag the image

```bash
docker tag extend-regional-payment-gateway:latest \
  <registry-url>/<your-namespace>/extend-regional-payment-gateway:latest
```

### 3c — Push the image

```bash
docker push <registry-url>/<your-namespace>/extend-regional-payment-gateway:latest
```

Once the push completes, the image is available in the AGS portal under your app's **Images** tab.

---

## Step 4 — Configure Environment Variables

In the AGS Admin Portal, open your app and go to the **Environment Variables** tab. Add each variable required by the service.

### Required variables

| Variable | Description |
|---|---|
| `AB_BASE_URL` | Your AGS environment base URL |
| `AB_CLIENT_ID` | AccelByte M2M client ID |
| `AB_CLIENT_SECRET` | AccelByte M2M client secret |
| `AB_NAMESPACE` | Your AccelByte namespace |
| `PUBLIC_BASE_URL` | The public HTTPS URL of this app (assigned by AGS after deployment) |

> Set `PUBLIC_BASE_URL` to the app URL shown in the portal after the first successful deployment. Until then, leave it as a placeholder and update it after Step 6.

### Provider variables

Add the env vars for each payment provider you want to enable. See the provider guides in [`docs/adapter/`](adapter/) for the full list of variables per provider:

- Xendit: [`XenditGuide.md`](adapter/XenditGuide.md)
- KOMOJU: [`KomojuGuide.md`](adapter/KomojuGuide.md)
- Generic HTTP: [`GenericGuide.md`](adapter/GenericGuide.md)

### Applying the configuration

After adding all variables, click **Restart and Apply**. AGS pushes the new configuration to the running instance. Any change to environment variables requires this step.

---

## Step 5 — Deploy the Image

1. In the Admin Portal, open your app and go to the **Deployments** tab.
2. Select the image version you pushed in Step 3.
3. Click **Deploy**.
4. Monitor the status:
   - `starting` — deployment is in progress
   - `running` — deployment succeeded and the service is healthy

> If the status shows `deployment failed` or `timeout`, check the app logs in the portal (Grafana link in the Monitoring tab) and verify that all required environment variables are set correctly.

---

## Step 6 — Verify the Deployment

Once the status shows `running`:

1. **Find the app URL** in the Admin Portal (shown on the app overview page).
2. **Open the Swagger UI:**
   ```
   https://<app-url>/payment/apidocs/
   ```
3. **Create a test payment intent** to confirm the service is responding:
   ```
   POST /payment/v1/payment/intent
   ```
4. **Update `PUBLIC_BASE_URL`** in the portal to the actual app URL, then click **Restart and Apply**.
5. **Update the webhook URLs** in your payment provider dashboards to point to:
   ```
   https://<app-url>/payment/v1/webhook/<providerId>
   ```

---

## Updating an Existing Deployment

### To deploy a new image version

1. Build and push a new tagged image (Steps 1 and 3).
2. In the portal, select the new image version under the **Deployments** tab.
3. Click **Deploy**.

### To update environment variables only

1. Edit the variables in the **Environment Variables** tab.
2. Click **Restart and Apply**. No new image upload is needed.

---

## App Lifecycle Reference

| Status | Meaning |
|---|---|
| `provisioning in progress` | App slot is being created |
| `undeployed` | App slot is ready; no image deployed yet |
| `starting` | Deployment is in progress |
| `running` | App is healthy and serving traffic |
| `stopping` | App is being stopped |
| `stopped` | App is stopped; no traffic served |
| `removing` | App is being deleted |
| `removed` | App has been deleted |

On persistent `deployment failed` or `timeout` errors:
1. Check the app logs via the Grafana link in the portal.
2. Verify all required environment variables are present and correct.
3. Rebuild and push the image, then redeploy.
4. If the issue persists, contact AccelByte support.

---

## Further Reading

- [AGS Extend Service Extension overview](https://docs.accelbyte.io/gaming-services/modules/foundations/extend/service-extension/)
- [Extend App Lifecycle](https://docs.accelbyte.io/gaming-services/modules/foundations/extend/extend-app-lifecycle/)
- [Configuring Environment Variables and Secrets](https://docs.accelbyte.io/gaming-services/modules/foundations/extend/configuring-envars/)
