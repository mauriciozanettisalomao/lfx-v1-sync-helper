# Data source ingest project

This is a [Meltano project](https://docs.meltano.com/concepts/project/) with a
standard directory layout created with `uv run meltano init "meltano"`.

## Important files

- `meltano.yml`: The main configuration file for the Meltano project. It
  defines all sources and targets supported.

- `load/target-nats-kv/`: Contains the custom target plugin for loading data
  into a NATS Key-Value store.
