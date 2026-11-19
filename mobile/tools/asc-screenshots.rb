#!/usr/bin/env ruby
# Upload store/screenshots/*.png to the App Store listing, in filename order.
#
#   ruby mobile/tools/asc-screenshots.rb            # 6.9" iPhone set
#
# Apple's upload is three steps per image: reserve (which hands back signed
# upload operations), PUT the bytes, then commit with an MD5 of the file. Miss
# the commit and the image sits there forever as "not uploaded".
require 'jwt'
require 'json'
require 'net/http'
require 'uri'
require 'openssl'
require 'digest'

KEY_ID = ENV.fetch('ASC_KEY_ID', 'H9PLG58DB9')
ISSUER = ENV.fetch('ASC_ISSUER', 'd5f5385b-5e34-4d70-925d-0debd3a3e090')
KEY_PATH = File.expand_path("~/.appstoreconnect/private_keys/AuthKey_#{KEY_ID}.p8")
APP_ID = ENV.fetch('GLIPH_APP_ID', '6805549904')
DISPLAY_TYPE = ENV.fetch('GLIPH_SCREENSHOT_TYPE', 'APP_IPHONE_67')
ROOT = File.expand_path('../..', __dir__)
BASE = 'https://api.appstoreconnect.apple.com'

def token
  key = OpenSSL::PKey::EC.new(File.read(KEY_PATH))
  JWT.encode({ iss: ISSUER, exp: Time.now.to_i + 900, aud: 'appstoreconnect-v1' },
             key, 'ES256', { kid: KEY_ID, typ: 'JWT' })
end

def call(method, path, body = nil)
  uri = URI("#{BASE}#{path}")
  klass = { get: Net::HTTP::Get, post: Net::HTTP::Post, patch: Net::HTTP::Patch,
            delete: Net::HTTP::Delete }[method]
  req = klass.new(uri)
  req['Authorization'] = "Bearer #{token}"
  req['Content-Type'] = 'application/json'
  req.body = JSON.dump(body) if body
  res = Net::HTTP.start(uri.host, uri.port, use_ssl: true) { |h| h.request(req) }
  abort "#{method.to_s.upcase} #{path} -> #{res.code}\n#{res.body}" unless res.code.to_i.between?(200, 299)
  res.body.to_s.empty? ? {} : JSON.parse(res.body)
end

version = call(:get, "/v1/apps/#{APP_ID}/appStoreVersions?limit=10")['data'].find do |v|
  !%w[READY_FOR_SALE REPLACED_WITH_NEW_VERSION].include?(v.dig('attributes', 'appStoreState'))
end
abort 'no editable version' unless version
loc = call(:get, "/v1/appStoreVersions/#{version['id']}/appStoreVersionLocalizations")['data'].first

sets = call(:get, "/v1/appStoreVersionLocalizations/#{loc['id']}/appScreenshotSets")['data']
set = sets.find { |s| s.dig('attributes', 'screenshotDisplayType') == DISPLAY_TYPE }
set ||= call(:post, '/v1/appScreenshotSets',
             { data: { type: 'appScreenshotSets',
                       attributes: { screenshotDisplayType: DISPLAY_TYPE },
                       relationships: { appStoreVersionLocalization:
                         { data: { type: 'appStoreVersionLocalizations', id: loc['id'] } } } } })['data']

existing = call(:get, "/v1/appScreenshotSets/#{set['id']}/appScreenshots")['data']
existing.each do |s|
  call(:delete, "/v1/appScreenshots/#{s['id']}")
  puts "removed old #{s.dig('attributes', 'fileName')}"
end

Dir[File.join(ROOT, 'store', 'screenshots', '*.png')].sort.each do |path|
  bytes = File.binread(path)
  name = File.basename(path)
  shot = call(:post, '/v1/appScreenshots',
              { data: { type: 'appScreenshots',
                        attributes: { fileSize: bytes.bytesize, fileName: name },
                        relationships: { appScreenshotSet:
                          { data: { type: 'appScreenshotSets', id: set['id'] } } } } })['data']

  shot.dig('attributes', 'uploadOperations').each do |op|
    uri = URI(op['url'])
    req = Net::HTTP::Put.new(uri)
    op['requestHeaders'].each { |h| req[h['name']] = h['value'] }
    req.body = bytes[op['offset'], op['length']]
    res = Net::HTTP.start(uri.host, uri.port, use_ssl: uri.scheme == 'https') { |h| h.request(req) }
    abort "upload chunk failed: #{res.code} #{res.body}" unless res.code.to_i.between?(200, 299)
  end

  call(:patch, "/v1/appScreenshots/#{shot['id']}",
       { data: { type: 'appScreenshots', id: shot['id'],
                 attributes: { uploaded: true, sourceFileChecksum: Digest::MD5.hexdigest(bytes) } } })
  puts "uploaded #{name}"
end
