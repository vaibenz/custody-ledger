# Deploying to AWS

The service runs as one container on **App Runner**. It connects to **RDS for PostgreSQL** and reads its secrets from **Secrets Manager**. App Runner gives HTTPS, autoscaling and rolling deploys without managing a cluster, which suits a single stateless API.

```
client ──HTTPS──> App Runner (custody-ledger image from ECR)
                        │  VPC connector
                        ▼
                  RDS PostgreSQL 16 (private subnets)
```

## 1. Database

```bash
aws rds create-db-instance \
  --db-instance-identifier custody-ledger \
  --engine postgres --engine-version 16 \
  --db-instance-class db.t4g.micro --allocated-storage 20 \
  --master-username ledger --manage-master-user-password \
  --no-publicly-accessible
```

`--manage-master-user-password` stores the password in Secrets Manager. Build a `DATABASE_URL` secret from it (with `sslmode=require`), plus an `ADMIN_TOKEN` secret:

```bash
aws secretsmanager create-secret --name custody-ledger/database-url \
  --secret-string 'postgres://ledger:<password>@<endpoint>:5432/postgres?sslmode=require'
aws secretsmanager create-secret --name custody-ledger/admin-token \
  --secret-string "$(openssl rand -hex 32)"
```

## 2. Image

```bash
ACCOUNT=$(aws sts get-caller-identity --query Account --output text)
REGION=ap-southeast-1
REPO=$ACCOUNT.dkr.ecr.$REGION.amazonaws.com/custody-ledger

aws ecr create-repository --repository-name custody-ledger
aws ecr get-login-password | docker login --username AWS --password-stdin $ACCOUNT.dkr.ecr.$REGION.amazonaws.com
docker build -t $REPO:latest . && docker push $REPO:latest
```

## 3. Service

In the App Runner console (or `aws apprunner create-service`):

* **Source:** the ECR image, port `8080`
* **Health check:** HTTP `/healthz`
* **Secrets:** `DATABASE_URL` and `ADMIN_TOKEN` from the two Secrets Manager entries
* **Networking:** a VPC connector into the RDS subnets, and a security group that allows 5432 from the connector

The server migrates the schema on startup under a Postgres advisory lock, so several instances starting at once is safe.

## Tear down

Delete the App Runner service, the RDS instance and the ECR repository so nothing keeps billing.
