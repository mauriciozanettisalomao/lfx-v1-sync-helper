ARG MELTANO_IMAGE=meltano/meltano:v4.0.2-python3.12-slim
FROM $MELTANO_IMAGE

WORKDIR /project

# Copy over Meltano project directory
COPY . .
RUN meltano install

# Don't allow changes to containerized project files
ENV MELTANO_PROJECT_READONLY=1

ENTRYPOINT ["meltano"]
