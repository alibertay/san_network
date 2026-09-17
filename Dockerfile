FROM python:3.12-slim

ENV PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    PIP_NO_CACHE_DIR=1

WORKDIR /app

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY . .

RUN useradd --create-home --uid 10001 san \
    && mkdir -p /data \
    && chown -R san:san /data /app
USER san

VOLUME ["/data"]
EXPOSE 8000 8765 8769 8770

CMD ["python", "run.py"]
