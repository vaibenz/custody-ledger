# Deploying to AWS

One EC2 instance runs the whole stack with Docker Compose: Postgres, the API, the outbox relay, and Caddy in front for HTTPS. Images live in ECR. GitHub Actions deploys every green build of `main` using OIDC, so no AWS access keys are stored anywhere.

```
GitHub Actions ──OIDC──> AWS role ──> push image to ECR
       │                                   │
       └── SSM Run Command ──> EC2 (t3.micro, Singapore)
                                 ├─ caddy   :443  https://<ip>.sslip.io  (Let's Encrypt)
                                 ├─ api     :8080
                                 ├─ relay
                                 └─ postgres (encrypted EBS volume)
```

The instance has no SSH port open; administration goes through SSM Session Manager. HTTPS works without buying a domain: `13-212-1-2.sslip.io` resolves to `13.212.1.2`, so Caddy can get a real certificate for it.

Everything is in [`aws/stack.yml`](aws/stack.yml) (CloudFormation) and [`../.github/workflows/deploy.yml`](../.github/workflows/deploy.yml).

## One time setup (about 10 minutes)

1. **AWS account.** Sign in to the AWS console and switch the region to **Asia Pacific (Singapore) ap-southeast-1**. Under Billing, create a budget alert (for example US$5) so nothing surprises you.

2. **Make a root API token.** Run `openssl rand -hex 32` and keep the output somewhere safe. This is the API's root credential.

3. **Create the stack.** Open CloudFormation, then choose Create stack, then With new resources.
   * Upload `deploy/aws/stack.yml`.
   * Name the stack exactly `custody-ledger`.
   * Paste your token into **AdminToken** and leave the other parameters as they are.
   * Tick the box acknowledging IAM resources, then create.
   * It takes about 5 minutes.

4. **Connect GitHub.** In the stack's **Outputs** tab, copy `DeployRoleArn`. In GitHub, go to the repo's Settings, then Secrets and variables, then Actions, then the **Variables** tab, and add:
   * `AWS_ROLE_ARN` = the ARN you copied

5. **Deploy.** In GitHub, go to Actions, choose **deploy**, then Run workflow on `main`. When it goes green, open the `URL` output with `/healthz` on the end. After this, every push to `main` that passes CI deploys automatically.

## Try it

```bash
URL=https://<your-ip-dashed>.sslip.io
ROOT=<your admin token>

# Root creates two operators (four-eyes needs two) and a customer key
OP1=$(curl -s -XPOST $URL/v1/admin/operators -H "Authorization: Bearer $ROOT" -d '{"name":"ops-1"}' | jq -r .token)
OP2=$(curl -s -XPOST $URL/v1/admin/operators -H "Authorization: Bearer $ROOT" -d '{"name":"ops-2"}' | jq -r .token)
ALICE=$(curl -s -XPOST $URL/v1/admin/keys -H "Authorization: Bearer $ROOT" -d '{"owner":"alice"}' | jq -r .key)

# Alice opens an account; an operator credits a deposit
A=$(curl -s -XPOST $URL/v1/accounts -H "Authorization: Bearer $ALICE" -d '{"asset":"BTC"}' | jq -r .id)
curl -s -XPOST $URL/v1/deposits -H "Authorization: Bearer $OP1" -H 'Idempotency-Key: dep-1' \
     -d "{\"account_id\":\"$A\",\"amount\":500000}"
curl -s $URL/v1/accounts/$A -H "Authorization: Bearer $ALICE"
```

## Costs

* A `t3.micro` instance, 20 GB of gp3 storage and ECR storage are within the AWS free tier on eligible accounts. Newer accounts get free plan credits instead.
* The public IPv4 address is billed by AWS at about US$0.005 an hour (roughly US$4 a month) beyond any free tier allowance.
* Check the Billing console after the first day to confirm.

## Operating it

* **Logs:** open Session Manager on the instance, then run `cd /opt/ledger && sudo docker compose logs -f api relay`.
* **Rollback:** run the deploy workflow from an older commit, or set `IMAGE_TAG` in `/opt/ledger/.env` and run `docker compose up -d`.
* **Database backups:** take an EBS snapshot of the instance volume. A production setup would move Postgres to RDS with automated backups and Multi-AZ, which this single instance trades away for cost.

## Tear down

1. Delete the images in the `custody-ledger` ECR repository. CloudFormation will not delete a repository that still holds images.
2. Delete the stack. This removes the instance, its disk, the IP address, the repository and the IAM roles, so billing stops.
