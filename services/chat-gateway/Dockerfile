# Build context: repository root.
FROM python:3.12.7-slim

ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 PIP_NO_CACHE_DIR=1 PIP_DISABLE_PIP_VERSION_CHECK=1
WORKDIR /app

COPY libs/python/aisre-common /tmp/aisre-common
COPY services/chat-gateway/requirements.txt .
RUN pip install /tmp/aisre-common -r requirements.txt && rm -rf /tmp/aisre-common

COPY services/chat-gateway/app ./app

USER 65534:65534
CMD ["python", "-m", "app.main"]
