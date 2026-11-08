#!/usr/bin/env ruby
# frozen_string_literal: true

require 'yaml'
require 'json'
require 'net/http'
require 'uri'
require 'set'

REPO_ROOT = File.expand_path(File.join(__dir__, '..', '..', '..'))
WORKFLOWS_DIR = File.join(REPO_ROOT, '.github', 'workflows')
EXCEPTIONS_PATH = File.join(REPO_ROOT, '.github', 'action-runtime-exceptions.json')
FORCE_VAR = 'FORCE_JAVASCRIPT_ACTIONS_TO_NODE24'
GITHUB_API = 'https://api.github.com'
ACTION_METADATA_CACHE = {}
PIN_COMMIT_CACHE = {}

class CheckFailure < StandardError; end

Occurrence = Struct.new(:workflow, :job, :step_name, :uses, :action_slug, :action_repo, :action_path, :ref, :scope, keyword_init: true)


def http_get(uri_str, headers = {})
  uri = URI(uri_str)
  attempts = 0

  begin
    attempts += 1
    req = Net::HTTP::Get.new(uri)
    headers.each { |k, v| req[k] = v }
    Net::HTTP.start(uri.host, uri.port, use_ssl: uri.scheme == 'https', open_timeout: 10, read_timeout: 20) do |http|
      return http.request(req)
    end
  rescue Timeout::Error, Errno::ECONNRESET, EOFError, OpenSSL::SSL::SSLError => e
    raise if attempts >= 3

    sleep(attempts)
    retry
  end
end


def github_headers
  headers = {
    'Accept' => 'application/vnd.github+json',
    'User-Agent' => 'bifrost-action-runtime-checker'
  }
  token = ENV['GITHUB_TOKEN'] || ENV['GH_TOKEN']
  headers['Authorization'] = "Bearer #{token}" if token && !token.empty?
  headers
end


def load_yaml(path)
  YAML.safe_load(File.read(path), permitted_classes: [], aliases: true) || {}
rescue Psych::Exception => e
  raise CheckFailure, "Failed to parse #{path}: #{e.message}"
end


def parse_uses(uses)
  slug, ref = uses.split('@', 2)
  raise CheckFailure, "Invalid uses reference: #{uses}" if slug.nil? || ref.nil?

  parts = slug.split('/')
  raise CheckFailure, "Invalid action slug: #{slug}" if parts.length < 2

  {
    action_slug: slug,
    action_repo: parts[0, 2].join('/'),
    action_path: parts.length > 2 ? parts[2..].join('/') : nil,
    ref: ref
  }
end


def workflow_occurrences(workflow_path)
  doc = load_yaml(workflow_path)
  jobs = doc.fetch('jobs', {})
  occurrences = []

  jobs.each do |job_name, job|
    steps = job.is_a?(Hash) ? job.fetch('steps', []) : []
    occurrences.concat(step_occurrences(workflow_path, job_name, steps))
  end

  [doc, occurrences]
end


# A local action (`uses: ./.github/actions/x`) has no ref of its own: the actions its
# composite steps use are checked in its place, attributed to its action.yml (TXE-6254).
def step_occurrences(source_path, job_name, steps, seen = Set.new)
  occurrences = []
  steps.each do |step|
    next unless step.is_a?(Hash) && step['uses'].is_a?(String)

    if step['uses'].start_with?('./')
      dir = File.expand_path(step['uses'], REPO_ROOT)
      action_file = %w[action.yml action.yaml].map { |f| File.join(dir, f) }.find { |f| File.file?(f) }
      raise CheckFailure, "#{source_path.sub("#{REPO_ROOT}/", '')}: no action.yml for local action #{step['uses']}" unless action_file
      next if seen.include?(action_file)

      runs = load_yaml(action_file).fetch('runs', {})
      next unless runs['using'] == 'composite'

      occurrences.concat(step_occurrences(action_file, job_name, runs.fetch('steps', []), seen | [action_file]))
      next
    end

    parsed = parse_uses(step['uses'])
    occurrences << Occurrence.new(
      workflow: source_path.sub("#{REPO_ROOT}/", ''),
      job: job_name,
      step_name: step['name'],
      uses: step['uses'],
      scope: step['env'].is_a?(Hash) && step['env'].key?(FORCE_VAR) ? 'step' : 'none',
      **parsed
    )
  end
  occurrences
end


def workflow_force_scope(doc)
  env = doc['env']
  env.is_a?(Hash) && env.key?(FORCE_VAR)
end


def fetch_action_metadata(occurrence)
  cache_key = [occurrence.action_repo, occurrence.ref, occurrence.action_path]
  return ACTION_METADATA_CACHE[cache_key] if ACTION_METADATA_CACHE.key?(cache_key)

  base = "https://raw.githubusercontent.com/#{occurrence.action_repo}/#{occurrence.ref}"
  candidates = []
  if occurrence.action_path && !occurrence.action_path.empty?
    candidates << "#{base}/#{occurrence.action_path}/action.yml"
    candidates << "#{base}/#{occurrence.action_path}/action.yaml"
  else
    candidates << "#{base}/action.yml"
    candidates << "#{base}/action.yaml"
  end

  body = nil
  fetched_url = nil
  candidates.each do |url|
    resp = http_get(url)
    next unless resp.is_a?(Net::HTTPSuccess)

    body = resp.body
    fetched_url = url
    break
  end

  raise CheckFailure, "Could not fetch action metadata for #{occurrence.uses}" unless body

  using_match = body.match(/^\s*using:\s*["']?([^"'\s]+)["']?\s*$/)
  raise CheckFailure, "Could not determine runs.using for #{occurrence.uses} from #{fetched_url}" unless using_match

  ACTION_METADATA_CACHE[cache_key] = {
    using: using_match[1],
    source: fetched_url
  }
end


def verify_pin_is_commit(occurrence)
  cache_key = [occurrence.action_repo, occurrence.ref]
  return if PIN_COMMIT_CACHE[cache_key]

  uri = "#{GITHUB_API}/repos/#{occurrence.action_repo}/commits/#{occurrence.ref}"
  resp = http_get(uri, github_headers)
  if resp.is_a?(Net::HTTPSuccess)
    PIN_COMMIT_CACHE[cache_key] = true
    return
  end

  raise CheckFailure,
        "#{occurrence.uses} does not pin a commit that GitHub recognizes for #{occurrence.action_repo} (HTTP #{resp.code}); use the release commit SHA, not an annotated tag object"
end


def load_exceptions
  data = JSON.parse(File.read(EXCEPTIONS_PATH))
  entries = data.fetch('exceptions')
  raise CheckFailure, "exceptions must be an array" unless entries.is_a?(Array)

  entries
end


def exception_key(entry)
  [entry.fetch('workflow'), entry.fetch('scope'), entry['step'], entry.fetch('action'), entry.fetch('ref')]
end


def find_exception(exceptions, occurrence)
  exceptions.find do |entry|
    next false unless entry['workflow'] == occurrence.workflow
    next false unless entry['action'] == occurrence.action_repo
    next false unless entry['ref'] == occurrence.ref

    if entry['scope'] == 'workflow'
      occurrence.scope == 'workflow'
    elsif entry['scope'] == 'step'
      occurrence.step_name == entry['step']
    else
      false
    end
  end
end


def stale_override_message(entry, runtime)
  target = entry['scope'] == 'step' ? "#{entry['workflow']} step #{entry['step']}" : entry['workflow']
  "Temporary override for #{target} is stale: #{entry['action']}@#{entry['ref']} now runs on #{runtime}, expected #{entry['expected_runtime']}. Remove the #{FORCE_VAR} override and delete the exception entry."
end


def main
  workflow_files = Dir.glob(File.join(WORKFLOWS_DIR, '*.yml')).sort

  all_occurrences = []
  workflow_force = {}
  workflow_files.each do |path|
    doc, occurrences = workflow_occurrences(path)
    rel_path = path.sub("#{REPO_ROOT}/", '')
    workflow_force[rel_path] = workflow_force_scope(doc)
    occurrences.each do |occ|
      occ.scope = 'workflow' if workflow_force[occ.workflow]
      all_occurrences << occ
    end
  end

  exceptions = load_exceptions
  failures = []
  used_exception_keys = Set.new

  if workflow_force['.github/workflows/scorecards.yml']
    failures << "#{FORCE_VAR} must never be defined at workflow scope in .github/workflows/scorecards.yml"
  end

  all_occurrences.each do |occurrence|
    next unless occurrence.ref.match?(/\A[0-9a-f]{40}\z/)

    begin
      verify_pin_is_commit(occurrence)
      metadata = fetch_action_metadata(occurrence)
    rescue CheckFailure => e
      failures << e.message
      next
    end

    runtime = metadata[:using]
    exception = find_exception(exceptions, occurrence)

    if runtime.start_with?('node')
      if exception.nil?
        next unless runtime == 'node20'

        failures << "#{occurrence.workflow}: #{occurrence.uses} runs on node20 but has no exception entry"
        next
      end

      used_exception_keys << exception_key(exception)
      expected_runtime = exception.fetch('expected_runtime')
      if runtime != expected_runtime
        failures << stale_override_message(exception, runtime)
      end

      if exception['scope'] == 'step' && occurrence.scope != 'step'
        failures << "#{occurrence.workflow} step #{occurrence.step_name} has exception for step-scoped override, but #{FORCE_VAR} is not defined at step scope"
      end

      if exception['scope'] == 'workflow' && !workflow_force[occurrence.workflow]
        failures << "#{occurrence.workflow} has workflow-scoped exception for #{occurrence.action_repo}, but #{FORCE_VAR} is not defined at workflow scope"
      end
    else
      unless exception.nil?
        used_exception_keys << exception_key(exception)
        failures << stale_override_message(exception, runtime)
      end
    end
  end

  exceptions.each do |entry|
    key = exception_key(entry)
    next if used_exception_keys.include?(key)

    failures << "Exception entry is unused or no longer matches a workflow step: #{entry.to_json}"
  end

  workflow_force.each do |workflow, has_force|
    next unless has_force

    matches = exceptions.select { |entry| entry['workflow'] == workflow && entry['scope'] == 'workflow' }
    if matches.empty?
      failures << "#{workflow} defines workflow-scoped #{FORCE_VAR} but has no workflow-scoped exception entries"
    end
  end

  step_force_pairs = []
  workflow_files.each do |path|
    doc = load_yaml(path)
    next unless doc['jobs'].is_a?(Hash)

    rel_path = path.sub("#{REPO_ROOT}/", '')
    doc['jobs'].each_value do |job|
      next unless job.is_a?(Hash) && job['steps'].is_a?(Array)
      job['steps'].each do |step|
        next unless step.is_a?(Hash)
        next unless step['env'].is_a?(Hash) && step['env'].key?(FORCE_VAR)
        step_force_pairs << [rel_path, step['name']]
      end
    end
  end

  step_force_pairs.each do |workflow, step_name|
    match = exceptions.find { |entry| entry['workflow'] == workflow && entry['scope'] == 'step' && entry['step'] == step_name }
    failures << "#{workflow} step #{step_name} defines step-scoped #{FORCE_VAR} but has no matching exception entry" unless match
  end

  if failures.empty?
    puts 'Action runtime guardrails passed.'
    exit 0
  end

  warn "Action runtime guardrails failed:"
  failures.uniq.each { |failure| warn "- #{failure}" }
  exit 1
end

main
